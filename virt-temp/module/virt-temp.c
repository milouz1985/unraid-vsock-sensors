// SPDX-License-Identifier: GPL-2.0-only

#include <linux/fs.h>
#include <linux/hwmon.h>
#include <linux/jhash.h>
#include <linux/jiffies.h>
#include <linux/list.h>
#include <linux/miscdevice.h>
#include <linux/module.h>
#include <linux/mutex.h>
#include <linux/platform_device.h>
#include <linux/slab.h>
#include <linux/string.h>
#include <linux/uaccess.h>

#define VIRT_TEMP_FAILSAFE_MILLIC 100000L
#define VIRT_TEMP_MAX_MILLIC 150000L
#define VIRT_TEMP_ID_SIZE 64
#define VIRT_TEMP_LABEL_SIZE 96
#define VIRT_TEMP_NAME_SIZE 64
#define VIRT_TEMP_WRITE_SIZE 256

struct virt_temp_sensor {
	struct list_head node;
	char id[VIRT_TEMP_ID_SIZE];
	char label[VIRT_TEMP_LABEL_SIZE];
	char name[VIRT_TEMP_NAME_SIZE];
	atomic_long_t temperature;
	unsigned long last_update;
	struct platform_device *platform;
	struct device *hwmon;
};

struct virt_temp_record {
	struct list_head node;
	char id[VIRT_TEMP_ID_SIZE];
	char label[VIRT_TEMP_LABEL_SIZE];
	long temperature;
};

/*
 * Each open file accumulates one family snapshot. Records have the textual
 * form "id<TAB>millidegrees<TAB>label". A final "commit<TAB>namespace"
 * applies them together. Closing without a commit changes nothing.
 */
struct virt_temp_session {
	struct list_head records;
	struct mutex lock;
	bool committed;
};

static unsigned int stale_timeout = 10;
module_param(stale_timeout, uint, 0644);
MODULE_PARM_DESC(stale_timeout,
		 "Seconds without an update before reporting 100 degrees Celsius");

static LIST_HEAD(virt_temp_sensors);
static DEFINE_MUTEX(virt_temp_lock);
static struct miscdevice virt_temp_misc;

static const u32 virt_temp_config[] = {
	HWMON_T_INPUT | HWMON_T_LABEL,
	0
};

static const struct hwmon_channel_info virt_temp_channel_info = {
	.type = hwmon_temp,
	.config = virt_temp_config,
};

static const struct hwmon_channel_info * const virt_temp_info[] = {
	&virt_temp_channel_info,
	NULL
};

static bool virt_temp_is_stale(const struct virt_temp_sensor *sensor)
{
	unsigned int timeout = max(stale_timeout, 1U);

	/* Cast before multiplying to keep the calculation in the jiffies domain. */
	return time_after(jiffies,
			  READ_ONCE(sensor->last_update) +
				  (unsigned long)timeout * HZ);
}

static struct virt_temp_sensor *virt_temp_find_sensor(const char *id)
{
	struct virt_temp_sensor *sensor;

	list_for_each_entry(sensor, &virt_temp_sensors, node) {
		if (!strcmp(sensor->id, id))
			return sensor;
	}
	return NULL;
}

static struct virt_temp_record *
virt_temp_find_record(struct virt_temp_session *session, const char *id)
{
	struct virt_temp_record *record;

	list_for_each_entry(record, &session->records, node) {
		if (!strcmp(record->id, id))
			return record;
	}
	return NULL;
}

static bool virt_temp_in_namespace(const char *id, const char *namespace)
{
	size_t length = strlen(namespace);

	return !strncmp(id, namespace, length) && id[length] == ':';
}

static umode_t virt_temp_is_visible(const void *data,
				    enum hwmon_sensor_types type,
				    u32 attr, int channel)
{
	if (type != hwmon_temp || channel != 0)
		return 0;
	if (attr == hwmon_temp_input || attr == hwmon_temp_label)
		return 0444;
	return 0;
}

static int virt_temp_read(struct device *dev, enum hwmon_sensor_types type,
			  u32 attr, int channel, long *value)
{
	struct virt_temp_sensor *sensor = dev_get_drvdata(dev);

	if (type != hwmon_temp || attr != hwmon_temp_input || channel != 0)
		return -EOPNOTSUPP;
	*value = virt_temp_is_stale(sensor) ? VIRT_TEMP_FAILSAFE_MILLIC :
						 atomic_long_read(&sensor->temperature);
	return 0;
}

static int virt_temp_read_string(struct device *dev,
				 enum hwmon_sensor_types type,
				 u32 attr, int channel, const char **str)
{
	struct virt_temp_sensor *sensor = dev_get_drvdata(dev);

	if (type != hwmon_temp || attr != hwmon_temp_label || channel != 0)
		return -EOPNOTSUPP;
	*str = sensor->label;
	return 0;
}

static const struct hwmon_ops virt_temp_ops = {
	.is_visible = virt_temp_is_visible,
	.read = virt_temp_read,
	.read_string = virt_temp_read_string,
};

static const struct hwmon_chip_info virt_temp_chip_info = {
	.ops = &virt_temp_ops,
	.info = virt_temp_info,
};

/* Build a stable, hwmon-safe name while retaining a readable family prefix. */
static void virt_temp_make_name(struct virt_temp_sensor *sensor)
{
	const char *family = virt_temp_in_namespace(sensor->id, "hba") ?
			     "hba" : "disk";
	u32 hash = jhash(sensor->id, strlen(sensor->id), 0);

	snprintf(sensor->name, sizeof(sensor->name), "virt_temp_%s_%08x",
		 family, hash);
}

/* Caller holds virt_temp_lock. */
static int virt_temp_register_sensor(struct virt_temp_sensor *sensor)
{
	struct device *hwmon;
	struct platform_device *platform;

	virt_temp_make_name(sensor);
	platform = platform_device_register_simple(sensor->name,
						  PLATFORM_DEVID_NONE, NULL, 0);
	if (IS_ERR(platform))
		return PTR_ERR(platform);
	sensor->platform = platform;
	hwmon = hwmon_device_register_with_info(&platform->dev,
						sensor->name, sensor,
						&virt_temp_chip_info, NULL);
	if (IS_ERR(hwmon)) {
		platform_device_unregister(platform);
		sensor->platform = NULL;
		return PTR_ERR(hwmon);
	}
	sensor->hwmon = hwmon;
	return 0;
}

static void virt_temp_unregister_sensor(struct virt_temp_sensor *sensor)
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

static int virt_temp_commit(struct virt_temp_session *session,
			    const char *namespace)
{
	struct virt_temp_sensor *sensor;
	struct virt_temp_record *record;
	int sensor_err;
	int err = 0;

	if (strcmp(namespace, "disk") && strcmp(namespace, "hba"))
		return -EINVAL;
	list_for_each_entry(record, &session->records, node) {
		if (!virt_temp_in_namespace(record->id, namespace))
			return -EINVAL;
	}

	mutex_lock(&virt_temp_lock);
	/*
	 * Apply sensors independently, as drivetemp does for SCSI add/remove
	 * events.  The commit marks a complete inventory so absent sensors can be
	 * removed; it is deliberately not a transaction across hwmon devices.
	 */
	list_for_each_entry(record, &session->records, node) {
		sensor = virt_temp_find_sensor(record->id);
		if (!sensor) {
			sensor = kzalloc(sizeof(*sensor), GFP_KERNEL);
			if (!sensor) {
				if (!err)
					err = -ENOMEM;
				continue;
			}
			strscpy(sensor->id, record->id, sizeof(sensor->id));
			strscpy(sensor->label, record->label, sizeof(sensor->label));
			atomic_long_set(&sensor->temperature, record->temperature);
			WRITE_ONCE(sensor->last_update, jiffies);
			sensor_err = virt_temp_register_sensor(sensor);
			if (sensor_err) {
				if (!err)
					err = sensor_err;
				kfree(sensor);
				continue;
			}
			list_add_tail(&sensor->node, &virt_temp_sensors);
			continue;
		}
		if (!sensor->hwmon || strcmp(sensor->label, record->label)) {
			/* Unregister first so no sysfs reader observes a changing label. */
			virt_temp_unregister_sensor(sensor);
			strscpy(sensor->label, record->label, sizeof(sensor->label));
			sensor_err = virt_temp_register_sensor(sensor);
			if (sensor_err && !err)
				err = sensor_err;
		}
		atomic_long_set(&sensor->temperature, record->temperature);
		WRITE_ONCE(sensor->last_update, jiffies);
	}

restart:
	list_for_each_entry(sensor, &virt_temp_sensors, node) {
		if (virt_temp_in_namespace(sensor->id, namespace) &&
		    !virt_temp_find_record(session, sensor->id)) {
			virt_temp_unregister_sensor(sensor);
			list_del(&sensor->node);
			kfree(sensor);
			goto restart;
		}
	}
	mutex_unlock(&virt_temp_lock);
	return err;
}

static int virt_temp_open(struct inode *inode, struct file *file)
{
	struct virt_temp_session *session;

	session = kzalloc(sizeof(*session), GFP_KERNEL);
	if (!session)
		return -ENOMEM;
	INIT_LIST_HEAD(&session->records);
	mutex_init(&session->lock);
	file->private_data = session;
	return 0;
}

static ssize_t virt_temp_write(struct file *file, const char __user *user,
			       size_t count, loff_t *offset)
{
	struct virt_temp_session *session = file->private_data;
	struct virt_temp_record *record;
	char *buffer = NULL, *cursor, *id, *temperature, *label;
	long value;
	int err;

	if (!count || count > VIRT_TEMP_WRITE_SIZE)
		return -EMSGSIZE;
	mutex_lock(&session->lock);
	if (session->committed) {
		err = -EPIPE;
		goto out;
	}
	buffer = memdup_user_nul(user, count);
	if (IS_ERR(buffer)) {
		err = PTR_ERR(buffer);
		buffer = NULL;
		goto out;
	}
	cursor = strim(buffer);

	if (!strncmp(cursor, "commit\t", 7)) {
		err = virt_temp_commit(session, cursor + 7);
		if (!err)
			session->committed = true;
		goto out;
	}

	id = strsep(&cursor, "\t");
	temperature = strsep(&cursor, "\t");
	label = cursor;
	if (!id || !*id || !temperature || !*temperature || !label || !*label ||
	    strchr(label, '\t') || strlen(id) >= VIRT_TEMP_ID_SIZE ||
	    strlen(label) >= VIRT_TEMP_LABEL_SIZE) {
		err = -EINVAL;
		goto out;
	}
	err = kstrtol(temperature, 10, &value);
	if (err)
		goto out;
	if (value < 0 || value > VIRT_TEMP_MAX_MILLIC) {
		err = -ERANGE;
		goto out;
	}

	record = virt_temp_find_record(session, id);
	if (!record) {
		record = kzalloc(sizeof(*record), GFP_KERNEL);
		if (!record) {
			err = -ENOMEM;
			goto out;
		}
		strscpy(record->id, id, sizeof(record->id));
		list_add_tail(&record->node, &session->records);
	}
	strscpy(record->label, label, sizeof(record->label));
	record->temperature = value;
	err = 0;
out:
	kfree(buffer);
	mutex_unlock(&session->lock);
	return err ? err : count;
}

static int virt_temp_release(struct inode *inode, struct file *file)
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

static const struct file_operations virt_temp_fops = {
	.owner = THIS_MODULE,
	.open = virt_temp_open,
	.write = virt_temp_write,
	.release = virt_temp_release,
};

static struct miscdevice virt_temp_misc = {
	.minor = MISC_DYNAMIC_MINOR,
	.name = "virt-temp",
	.fops = &virt_temp_fops,
	.mode = 0600,
};

static int __init virt_temp_init(void)
{
	return misc_register(&virt_temp_misc);
}

static void __exit virt_temp_exit(void)
{
	struct virt_temp_sensor *sensor, *next;

	misc_deregister(&virt_temp_misc);
	list_for_each_entry_safe(sensor, next, &virt_temp_sensors, node) {
		virt_temp_unregister_sensor(sensor);
		list_del(&sensor->node);
		kfree(sensor);
	}
}

module_init(virt_temp_init);
module_exit(virt_temp_exit);

MODULE_AUTHOR("François HOYEZ");
MODULE_DESCRIPTION("Dynamic virtual hwmon temperature devices");
MODULE_LICENSE("GPL");
