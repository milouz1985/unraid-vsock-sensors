// SPDX-License-Identifier: GPL-2.0-only

#include <linux/fs.h>
#include <linux/hwmon.h>
#include <linux/jiffies.h>
#include <linux/list.h>
#include <linux/miscdevice.h>
#include <linux/module.h>
#include <linux/mutex.h>
#include <linux/platform_device.h>
#include <linux/slab.h>
#include <linux/string.h>
#include <linux/uaccess.h>

#define FAILSAFE_MILLIC 100000L
#define MAX_MILLIC 150000L
#define MAX_STALE_TIMEOUT 300U
#define ID_SIZE 64
#define LABEL_SIZE 96
#define MAX_RECORDS 1024
#define MAX_WRITE_SIZE 256

struct virt_temp_sensor {
	char id[ID_SIZE];
	char label[LABEL_SIZE];
	atomic_long_t temperature;
	unsigned long last_update;
};

struct virt_temp_family {
	const char *namespace;
	const char *hwmon_name;
	/* Serializes inventory replacement and updates for this family only. */
	struct mutex *lock;
	struct platform_device *platform;
	struct device *hwmon;
	struct virt_temp_sensor *sensors;
	unsigned int count;
	u32 *config;
	struct hwmon_channel_info channel_info;
	const struct hwmon_channel_info *info[2];
	struct hwmon_chip_info chip_info;
};

struct virt_temp_record {
	struct list_head node;
	char id[ID_SIZE];
	char label[LABEL_SIZE];
	long temperature;
};

/* Each open file stages one complete configure or update operation. */
struct virt_temp_session {
	struct list_head records;
	struct mutex lock;
	unsigned int count;
	bool applied;
};

static unsigned int stale_timeout = 10;
static DEFINE_MUTEX(storage_lock);
static DEFINE_MUTEX(hba_lock);
static struct virt_temp_family storage = {
	.namespace = "disk", .hwmon_name = "unraid_storage",
	.lock = &storage_lock,
};
static struct virt_temp_family hba = {
	.namespace = "hba", .hwmon_name = "unraid_hba",
	.lock = &hba_lock,
};

static int set_stale_timeout(const char *value,
			     const struct kernel_param *parameter)
{
	unsigned int timeout;
	int err = kstrtouint(value, 0, &timeout);

	if (err)
		return err;
	if (!timeout || timeout > MAX_STALE_TIMEOUT)
		return -ERANGE;
	return param_set_uint(value, parameter);
}

static const struct kernel_param_ops stale_timeout_ops = {
	.set = set_stale_timeout, .get = param_get_uint,
};
module_param_cb(stale_timeout, &stale_timeout_ops, &stale_timeout, 0644);
MODULE_PARM_DESC(stale_timeout,
		 "Seconds without an update before reporting 100 degrees Celsius (1-300)");

static struct virt_temp_family *find_family(const char *namespace)
{
	if (!strcmp(namespace, storage.namespace))
		return &storage;
	if (!strcmp(namespace, hba.namespace))
		return &hba;
	return NULL;
}

static struct virt_temp_record *find_record(struct virt_temp_session *session,
					    const char *id)
{
	struct virt_temp_record *record;

	list_for_each_entry(record, &session->records, node)
		if (!strcmp(record->id, id))
			return record;
	return NULL;
}

static struct virt_temp_sensor *find_sensor(struct virt_temp_family *family,
					    const char *id)
{
	unsigned int channel;

	for (channel = 0; channel < family->count; channel++)
		if (!strcmp(family->sensors[channel].id, id))
			return &family->sensors[channel];
	return NULL;
}

static bool in_namespace(const char *id, const char *namespace)
{
	size_t length = strlen(namespace);

	return !strncmp(id, namespace, length) && id[length] == ':';
}

static bool is_stale(const struct virt_temp_sensor *sensor)
{
	unsigned long updated = smp_load_acquire(&sensor->last_update);

	return time_after(jiffies, updated +
			  (unsigned long)READ_ONCE(stale_timeout) * HZ);
}

static umode_t is_visible(const void *data, enum hwmon_sensor_types type,
			  u32 attr, int channel)
{
	const struct virt_temp_family *family = data;

	if (type != hwmon_temp || channel < 0 ||
	    channel >= (int)family->count)
		return 0;
	return attr == hwmon_temp_input || attr == hwmon_temp_label ? 0444 : 0;
}

static int read_value(struct device *dev, enum hwmon_sensor_types type,
		      u32 attr, int channel, long *value)
{
	struct virt_temp_family *family = dev_get_drvdata(dev);
	struct virt_temp_sensor *sensor;

	if (type != hwmon_temp || attr != hwmon_temp_input || channel < 0 ||
	    channel >= (int)family->count)
		return -EOPNOTSUPP;
	sensor = &family->sensors[channel];
	*value = is_stale(sensor) ? FAILSAFE_MILLIC :
				       atomic_long_read(&sensor->temperature);
	return 0;
}

static int read_label(struct device *dev, enum hwmon_sensor_types type,
		      u32 attr, int channel, const char **str)
{
	struct virt_temp_family *family = dev_get_drvdata(dev);

	if (type != hwmon_temp || attr != hwmon_temp_label || channel < 0 ||
	    channel >= (int)family->count)
		return -EOPNOTSUPP;
	*str = family->sensors[channel].label;
	return 0;
}

static const struct hwmon_ops hwmon_ops = {
	.is_visible = is_visible, .read = read_value, .read_string = read_label,
};

static void unregister_hwmon(struct virt_temp_family *family)
{
	if (family->hwmon) {
		hwmon_device_unregister(family->hwmon);
		family->hwmon = NULL;
	}
}

static void free_inventory(struct virt_temp_family *family)
{
	kfree(family->config);
	kfree(family->sensors);
	family->config = NULL;
	family->sensors = NULL;
	family->count = 0;
}

static int build_inventory(struct virt_temp_family *family,
			   struct virt_temp_session *session)
{
	struct virt_temp_record *record;
	unsigned int channel = 0;

	if (!session->count)
		return 0;
	family->sensors = kcalloc(session->count, sizeof(*family->sensors),
				  GFP_KERNEL);
	family->config = kcalloc(session->count + 1, sizeof(*family->config),
				 GFP_KERNEL);
	if (!family->sensors || !family->config) {
		free_inventory(family);
		return -ENOMEM;
	}
	family->count = session->count;
	list_for_each_entry(record, &session->records, node) {
		struct virt_temp_sensor *sensor = &family->sensors[channel];

		strscpy(sensor->id, record->id, sizeof(sensor->id));
		strscpy(sensor->label, record->label, sizeof(sensor->label));
		atomic_long_set(&sensor->temperature, record->temperature);
		smp_store_release(&sensor->last_update, jiffies);
		family->config[channel++] = HWMON_T_INPUT | HWMON_T_LABEL;
	}
	family->channel_info.type = hwmon_temp;
	family->channel_info.config = family->config;
	family->info[0] = &family->channel_info;
	family->chip_info.ops = &hwmon_ops;
	family->chip_info.info = family->info;
	return 0;
}

static int configure(struct virt_temp_session *session,
		     struct virt_temp_family *family)
{
	struct virt_temp_family replacement = {
		.namespace = family->namespace,
		.hwmon_name = family->hwmon_name,
		.lock = family->lock,
		.platform = family->platform,
	};
	int err = build_inventory(&replacement, session);

	if (err)
		return err;
	mutex_lock(family->lock);
	/*
	 * hwmon_device_unregister() removes the sysfs device and drains in-flight
	 * hwmon callbacks before returning. It must therefore precede any change
	 * or free of sensors/count. This lifetime guarantee is also why read_value
	 * and read_label do not need the family mutex.
	 */
	unregister_hwmon(family);
	free_inventory(family);
	*family = replacement;
	/* Rebase the descriptor pointers copied from the stack replacement. */
	family->channel_info.config = family->config;
	family->info[0] = &family->channel_info;
	family->info[1] = NULL;
	family->chip_info.info = family->info;
	if (family->count) {
		family->hwmon = hwmon_device_register_with_info(
			&family->platform->dev, family->hwmon_name, family,
			&family->chip_info, NULL);
		if (IS_ERR(family->hwmon)) {
			err = PTR_ERR(family->hwmon);
			family->hwmon = NULL;
			free_inventory(family);
		}
	}
	mutex_unlock(family->lock);
	return err;
}

static int update(struct virt_temp_session *session,
		  struct virt_temp_family *family)
{
	struct virt_temp_record *record;
	struct virt_temp_sensor *sensor;

	mutex_lock(family->lock);
	list_for_each_entry(record, &session->records, node) {
		sensor = find_sensor(family, record->id);
		if (!sensor || strcmp(sensor->label, record->label)) {
			mutex_unlock(family->lock);
			return -ESTALE;
		}
	}
	list_for_each_entry(record, &session->records, node) {
		sensor = find_sensor(family, record->id);
		atomic_long_set(&sensor->temperature, record->temperature);
		smp_store_release(&sensor->last_update, jiffies);
	}
	mutex_unlock(family->lock);
	return 0;
}

static int apply(struct virt_temp_session *session, const char *operation,
		 const char *namespace)
{
	struct virt_temp_family *family = find_family(namespace);
	struct virt_temp_record *record;

	if (!family)
		return -EINVAL;
	list_for_each_entry(record, &session->records, node)
		if (!in_namespace(record->id, namespace))
			return -EINVAL;
	if (!strcmp(operation, "configure"))
		return configure(session, family);
	if (!strcmp(operation, "commit"))
		return update(session, family);
	return -EINVAL;
}

static int device_open(struct inode *inode, struct file *file)
{
	struct virt_temp_session *session = kzalloc(sizeof(*session), GFP_KERNEL);

	if (!session)
		return -ENOMEM;
	INIT_LIST_HEAD(&session->records);
	mutex_init(&session->lock);
	file->private_data = session;
	return 0;
}

static ssize_t device_write(struct file *file, const char __user *user,
			    size_t count, loff_t *offset)
{
	struct virt_temp_session *session = file->private_data;
	struct virt_temp_record *record;
	char *buffer = NULL, *cursor, *first, *temperature, *label;
	long value;
	int err;

	if (!count || count > MAX_WRITE_SIZE)
		return -EMSGSIZE;
	mutex_lock(&session->lock);
	if (session->applied) {
		err = -EPIPE;
		goto out;
	}
	buffer = memdup_user_nul(user, count);
	if (IS_ERR(buffer)) {
		err = PTR_ERR(buffer);
		buffer = NULL;
		goto out;
	}
	if (memchr(buffer, '\0', count)) {
		err = -EINVAL;
		goto out;
	}
	cursor = strim(buffer);
	first = strsep(&cursor, "\t");
	if ((!strcmp(first, "configure") || !strcmp(first, "commit")) &&
	    cursor && *cursor && !strchr(cursor, '\t')) {
		err = apply(session, first, cursor);
		if (!err)
			session->applied = true;
		goto out;
	}
	temperature = strsep(&cursor, "\t");
	label = cursor;
	if (!first || !*first || !temperature || !*temperature ||
	    !label || !*label || strpbrk(first, "\t\r\n") ||
	    strpbrk(label, "\t\r\n") || strlen(first) >= ID_SIZE ||
	    strlen(label) >= LABEL_SIZE) {
		err = -EINVAL;
		goto out;
	}
	err = kstrtol(temperature, 10, &value);
	if (err)
		goto out;
	if (value < 0 || value > MAX_MILLIC) {
		err = -ERANGE;
		goto out;
	}
	record = find_record(session, first);
	if (!record) {
		if (session->count >= MAX_RECORDS) {
			err = -ENOSPC;
			goto out;
		}
		record = kzalloc(sizeof(*record), GFP_KERNEL);
		if (!record) {
			err = -ENOMEM;
			goto out;
		}
		strscpy(record->id, first, sizeof(record->id));
		list_add_tail(&record->node, &session->records);
		session->count++;
	}
	strscpy(record->label, label, sizeof(record->label));
	record->temperature = value;
	err = 0;
out:
	kfree(buffer);
	mutex_unlock(&session->lock);
	return err ? err : count;
}

static int device_release(struct inode *inode, struct file *file)
{
	struct virt_temp_session *session = file->private_data;
	struct virt_temp_record *record, *next;

	list_for_each_entry_safe(record, next, &session->records, node) {
		list_del(&record->node);
		kfree(record);
	}
	mutex_destroy(&session->lock);
	kfree(session);
	return 0;
}

static const struct file_operations device_fops = {
	.owner = THIS_MODULE, .open = device_open, .write = device_write,
	.release = device_release,
};
static struct miscdevice control_device = {
	.minor = MISC_DYNAMIC_MINOR, .name = "virt-temp",
	.fops = &device_fops, .mode = 0600,
};

static int __init virt_temp_init(void)
{
	int err;

	storage.platform = platform_device_register_simple(
		"virt_temp_storage", PLATFORM_DEVID_NONE, NULL, 0);
	if (IS_ERR(storage.platform))
		return PTR_ERR(storage.platform);
	hba.platform = platform_device_register_simple(
		"virt_temp_hba", PLATFORM_DEVID_NONE, NULL, 0);
	if (IS_ERR(hba.platform)) {
		err = PTR_ERR(hba.platform);
		platform_device_unregister(storage.platform);
		return err;
	}
	err = misc_register(&control_device);
	if (err) {
		platform_device_unregister(hba.platform);
		platform_device_unregister(storage.platform);
	}
	return err;
}

static void __exit virt_temp_exit(void)
{
	misc_deregister(&control_device);
	unregister_hwmon(&hba);
	free_inventory(&hba);
	unregister_hwmon(&storage);
	free_inventory(&storage);
	platform_device_unregister(hba.platform);
	platform_device_unregister(storage.platform);
}

module_init(virt_temp_init);
module_exit(virt_temp_exit);
MODULE_AUTHOR("François HOYEZ");
MODULE_DESCRIPTION("Fixed-inventory virtual hwmon temperature channels");
MODULE_LICENSE("GPL");
