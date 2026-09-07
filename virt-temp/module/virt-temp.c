// SPDX-License-Identifier: GPL-2.0-only

#include <linux/fs.h>
#include <linux/hwmon.h>
#include <linux/jiffies.h>
#include <linux/kernel.h>
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
#define SENSOR_NAME_SIZE (sizeof("unraid_disk_") + 2 * (ID_SIZE - 1))
#define MAX_RECORDS 1024
#define MAX_WRITE_SIZE 256

struct virt_temp_sensor {
	char id[ID_SIZE];
	char label[LABEL_SIZE];
	char name[SENSOR_NAME_SIZE];
	atomic_long_t temperature;
	unsigned long last_update;
	/*
	 * Keep one stable platform/hwmon device per ID. Aggregated tempN channels
	 * are positional: topology changes either renumber them or require stale
	 * 100-degree tombstones for removed sensors.
	 */
	struct platform_device *platform;
	struct device *hwmon;
};

struct virt_temp_inventory {
	struct virt_temp_sensor *sensors;
	unsigned int count;
};

struct virt_temp_family {
	const char *namespace;
	/* Serializes inventory replacement and updates for this family only. */
	struct mutex *lock;
	struct virt_temp_inventory *inventory;
};

struct virt_temp_record {
	struct list_head node;
	char id[ID_SIZE];
	char label[LABEL_SIZE];
	long temperature;
};

/*
 * Each open file stages samples for one operation. Configure replaces a
 * family's complete inventory; commit may update any subset of its sensors.
 */
struct virt_temp_session {
	struct list_head records;
	struct mutex lock;
	unsigned int count;
	bool applied;
};

static unsigned int stale_timeout = 10;
static DEFINE_MUTEX(storage_lock);
static DEFINE_MUTEX(hba_lock);
static struct virt_temp_family disk_family = {
	.namespace = "disk", .lock = &storage_lock,
};
static struct virt_temp_family hba_family = {
	.namespace = "hba", .lock = &hba_lock,
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
	if (!strcmp(namespace, disk_family.namespace))
		return &disk_family;
	if (!strcmp(namespace, hba_family.namespace))
		return &hba_family;
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

static struct virt_temp_sensor *find_sensor(struct virt_temp_inventory *inventory,
					    const char *id)
{
	unsigned int index;

	for (index = 0; index < inventory->count; index++)
		if (!strcmp(inventory->sensors[index].id, id))
			return &inventory->sensors[index];
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
	if (type != hwmon_temp || channel != 0)
		return 0;
	return attr == hwmon_temp_input || attr == hwmon_temp_label ? 0444 : 0;
}

static int read_value(struct device *dev, enum hwmon_sensor_types type,
		      u32 attr, int channel, long *value)
{
	struct virt_temp_sensor *sensor = dev_get_drvdata(dev);

	if (type != hwmon_temp || attr != hwmon_temp_input || channel != 0)
		return -EOPNOTSUPP;
	*value = is_stale(sensor) ? FAILSAFE_MILLIC :
				       atomic_long_read(&sensor->temperature);
	return 0;
}

static int read_label(struct device *dev, enum hwmon_sensor_types type,
		      u32 attr, int channel, const char **str)
{
	struct virt_temp_sensor *sensor = dev_get_drvdata(dev);

	if (type != hwmon_temp || attr != hwmon_temp_label || channel != 0)
		return -EOPNOTSUPP;
	*str = sensor->label;
	return 0;
}

static const struct hwmon_ops hwmon_ops = {
	.is_visible = is_visible, .read = read_value, .read_string = read_label,
};

static const u32 temp_config[] = {
	HWMON_T_INPUT | HWMON_T_LABEL,
	0,
};
static const struct hwmon_channel_info temp_channel_info = {
	.type = hwmon_temp,
	.config = temp_config,
};
static const struct hwmon_channel_info * const temp_info[] = {
	&temp_channel_info,
	NULL,
};
static const struct hwmon_chip_info temp_chip_info = {
	.ops = &hwmon_ops,
	.info = temp_info,
};

static void make_sensor_name(struct virt_temp_sensor *sensor,
			     const struct virt_temp_family *family)
{
	static const char hex[] = "0123456789abcdef";
	const unsigned char *id = (const unsigned char *)sensor->id;
	size_t offset = scnprintf(sensor->name, sizeof(sensor->name),
				  "unraid_%s_", family->namespace);

	/* Hex is collision-free, hwmon-safe, and stable as a platform path. */
	while (*id && offset + 2 < sizeof(sensor->name)) {
		sensor->name[offset++] = hex[*id >> 4];
		sensor->name[offset++] = hex[*id & 0x0f];
		id++;
	}
	sensor->name[offset] = '\0';
}

static void unregister_sensor(struct virt_temp_sensor *sensor)
{
	if (sensor->hwmon) {
		hwmon_device_unregister(sensor->hwmon);
		sensor->hwmon = NULL;
	}
	if (sensor->platform) {
		platform_device_unregister(sensor->platform);
		sensor->platform = NULL;
	}
}

static void unregister_inventory(struct virt_temp_inventory *inventory)
{
	unsigned int index;

	if (!inventory)
		return;
	for (index = 0; index < inventory->count; index++)
		unregister_sensor(&inventory->sensors[index]);
}

static void free_inventory(struct virt_temp_inventory *inventory)
{
	if (!inventory)
		return;
	kfree(inventory->sensors);
	kfree(inventory);
}

static int register_sensor(struct virt_temp_family *family,
			   struct virt_temp_sensor *sensor)
{
	int err;

	make_sensor_name(sensor, family);
	sensor->platform = platform_device_register_simple(sensor->name,
							  PLATFORM_DEVID_NONE,
							  NULL, 0);
	if (IS_ERR(sensor->platform)) {
		err = PTR_ERR(sensor->platform);
		sensor->platform = NULL;
		return err;
	}
	sensor->hwmon = hwmon_device_register_with_info(
		&sensor->platform->dev, sensor->name, sensor,
		&temp_chip_info, NULL);
	if (IS_ERR(sensor->hwmon)) {
		err = PTR_ERR(sensor->hwmon);
		sensor->hwmon = NULL;
		platform_device_unregister(sensor->platform);
		sensor->platform = NULL;
		return err;
	}
	return 0;
}

static int register_inventory(struct virt_temp_family *family,
			      struct virt_temp_inventory *inventory)
{
	unsigned int index;
	int err;

	for (index = 0; index < inventory->count; index++) {
		err = register_sensor(family, &inventory->sensors[index]);
		if (err) {
			while (index > 0) {
				index--;
				unregister_sensor(&inventory->sensors[index]);
			}
			return err;
		}
	}
	return 0;
}

static bool inventory_is_registered(const struct virt_temp_inventory *inventory)
{
	unsigned int index;

	if (!inventory)
		return false;
	for (index = 0; index < inventory->count; index++)
		if (!inventory->sensors[index].platform ||
		    !inventory->sensors[index].hwmon)
			return false;
	return true;
}

static struct virt_temp_inventory *build_inventory(
	struct virt_temp_session *session)
{
	struct virt_temp_inventory *inventory;
	struct virt_temp_record *record;
	unsigned int index = 0;

	inventory = kzalloc(sizeof(*inventory), GFP_KERNEL);
	if (!inventory)
		return ERR_PTR(-ENOMEM);
	if (!session->count)
		return inventory;
	inventory->sensors = kcalloc(session->count,
				     sizeof(*inventory->sensors), GFP_KERNEL);
	if (!inventory->sensors) {
		free_inventory(inventory);
		return ERR_PTR(-ENOMEM);
	}
	inventory->count = session->count;
	list_for_each_entry(record, &session->records, node) {
		struct virt_temp_sensor *sensor = &inventory->sensors[index];

		strscpy(sensor->id, record->id, sizeof(sensor->id));
		strscpy(sensor->label, record->label, sizeof(sensor->label));
		atomic_long_set(&sensor->temperature, record->temperature);
		smp_store_release(&sensor->last_update, jiffies);
		index++;
	}
	return inventory;
}

static int configure(struct virt_temp_session *session,
		     struct virt_temp_family *family)
{
	struct virt_temp_inventory *replacement = build_inventory(session);
	struct virt_temp_inventory *previous;
	int rollback_err;
	int err;

	if (IS_ERR(replacement))
		return PTR_ERR(replacement);
	mutex_lock(family->lock);
	/*
	 * hwmon_device_unregister() removes the sysfs device and drains in-flight
	 * hwmon callbacks before returning. Every old hwmon and platform device
	 * must therefore be removed before their sensor storage is freed. This
	 * lifetime guarantee is also why read_value and read_label do not need the
	 * family mutex.
	 */
	previous = family->inventory;
	unregister_inventory(previous);
	err = register_inventory(family, replacement);
	if (!err) {
		family->inventory = replacement;
		free_inventory(previous);
	} else {
		/*
		 * Keep working, stale-safe hwmon devices if registering the new
		 * topology fails. The userspace error makes the agent retry the new
		 * inventory, while the restored sensors naturally reach their failsafe.
		 */
		free_inventory(replacement);
		rollback_err = previous ? register_inventory(family, previous) : 0;
		if (rollback_err)
			pr_err("failed to restore %s hwmon inventory: %d\n",
			       family->namespace, rollback_err);
	}
	mutex_unlock(family->lock);
	return err;
}

static int update(struct virt_temp_session *session,
		  struct virt_temp_family *family)
{
	struct virt_temp_record *record;
	struct virt_temp_sensor *sensor;
	struct virt_temp_inventory *inventory;

	mutex_lock(family->lock);
	inventory = family->inventory;
	/*
	 * A failed configure followed by a failed rollback can leave the previous
	 * inventory allocated but without registered sensor devices. Force
	 * userspace to configure it again. An empty inventory intentionally has no
	 * devices, so it remains a valid commit target.
	 */
	if (!inventory_is_registered(inventory)) {
		mutex_unlock(family->lock);
		return -ESTALE;
	}
	list_for_each_entry(record, &session->records, node) {
		sensor = find_sensor(inventory, record->id);
		if (!sensor || strcmp(sensor->label, record->label)) {
			mutex_unlock(family->lock);
			return -ESTALE;
		}
	}
	list_for_each_entry(record, &session->records, node) {
		sensor = find_sensor(inventory, record->id);
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
	char *buffer = NULL, *cursor, *kind, *id, *temperature, *label;
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
	/*
	 * Parse the text protocol emitted by encodeHWMonSamples() in hwmon_device.go.
	 * Every write on an open file is either a tab-separated sample:
	 *
	 *   sample\t<stable ID>\t<temperature in milli-Celsius>\t<label>\n
	 *
	 * or the command that atomically applies the session's samples:
	 *
	 *   configure\t<namespace>\n
	 *   commit\t<namespace>\n
	 *
	 * IDs and labels are deliberately forbidden from containing tabs or
	 * newlines, so no quoting or escaping is required here.
	 */
	cursor = strim(buffer);
	kind = strsep(&cursor, "\t");
	if ((!strcmp(kind, "configure") || !strcmp(kind, "commit")) &&
	    cursor && *cursor && !strchr(cursor, '\t')) {
		err = apply(session, kind, cursor);
		if (!err)
			session->applied = true;
		goto out;
	}
	if (strcmp(kind, "sample")) {
		err = -EINVAL;
		goto out;
	}
	id = strsep(&cursor, "\t");
	temperature = strsep(&cursor, "\t");
	label = cursor;
	if (!id || !*id || !temperature || !*temperature || !label || !*label ||
	    strpbrk(id, "\t\r\n") || strpbrk(label, "\t\r\n") ||
	    strlen(id) >= ID_SIZE ||
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
	record = find_record(session, id);
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
		strscpy(record->id, id, sizeof(record->id));
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
	return misc_register(&control_device);
}

static void __exit virt_temp_exit(void)
{
	misc_deregister(&control_device);
	unregister_inventory(hba_family.inventory);
	free_inventory(hba_family.inventory);
	unregister_inventory(disk_family.inventory);
	free_inventory(disk_family.inventory);
}

module_init(virt_temp_init);
module_exit(virt_temp_exit);
MODULE_AUTHOR("François HOYEZ");
MODULE_DESCRIPTION("Stable per-sensor virtual hwmon temperature devices");
MODULE_LICENSE("GPL");
