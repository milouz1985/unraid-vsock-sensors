// SPDX-License-Identifier: GPL-2.0-only

#include <linux/fs.h>
#include <linux/hwmon.h>
#include <linux/jiffies.h>
#include <linux/list.h>
#include <linux/miscdevice.h>
#include <linux/module.h>
#include <linux/mutex.h>
#include <linux/slab.h>
#include <linux/string.h>
#include <linux/uaccess.h>

#define VIRT_TEMP_FAILSAFE_MILLIC 100000L
#define VIRT_TEMP_MAX_MILLIC 150000L
#define VIRT_TEMP_MAX_CHANNELS 256
#define VIRT_TEMP_ID_SIZE 64
#define VIRT_TEMP_LABEL_SIZE 96
#define VIRT_TEMP_WRITE_SIZE 256

struct virt_temp_sensor {
	struct list_head node;
	char id[VIRT_TEMP_ID_SIZE];
	char label[VIRT_TEMP_LABEL_SIZE];
	atomic_long_t temperature;
	unsigned long last_update;
	unsigned int channel;
	bool active;
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
	bool committed;
};

static unsigned int stale_timeout = 10;
module_param(stale_timeout, uint, 0644);
MODULE_PARM_DESC(stale_timeout,
		 "Seconds without an update before reporting 100 degrees Celsius");

static LIST_HEAD(virt_temp_sensors);
static DEFINE_MUTEX(virt_temp_lock);
static struct miscdevice virt_temp_misc;
static struct device *virt_temp_hwmon;
static unsigned int virt_temp_next_channel;
static u32 *virt_temp_config;

static struct hwmon_channel_info virt_temp_channel_info = {
	.type = hwmon_temp,
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

static struct virt_temp_sensor *virt_temp_find_channel(int channel)
{
	struct virt_temp_sensor *sensor;

	list_for_each_entry(sensor, &virt_temp_sensors, node) {
		if (sensor->active && sensor->channel == channel)
			return sensor;
	}
	return NULL;
}

static bool virt_temp_has_active_sensor(void)
{
	struct virt_temp_sensor *sensor;

	list_for_each_entry(sensor, &virt_temp_sensors, node) {
		if (sensor->active)
			return true;
	}
	return false;
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
	if (type != hwmon_temp || !virt_temp_find_channel(channel))
		return 0;
	if (attr == hwmon_temp_input || attr == hwmon_temp_label)
		return 0444;
	return 0;
}

static int virt_temp_read(struct device *dev, enum hwmon_sensor_types type,
			  u32 attr, int channel, long *value)
{
	struct virt_temp_sensor *sensor;

	if (type != hwmon_temp || attr != hwmon_temp_input)
		return -EOPNOTSUPP;
	sensor = virt_temp_find_channel(channel);
	if (!sensor)
		return -ENODATA;
	*value = virt_temp_is_stale(sensor) ? VIRT_TEMP_FAILSAFE_MILLIC :
						 atomic_long_read(&sensor->temperature);
	return 0;
}

static int virt_temp_read_string(struct device *dev,
				 enum hwmon_sensor_types type,
				 u32 attr, int channel, const char **str)
{
	struct virt_temp_sensor *sensor;

	if (type != hwmon_temp || attr != hwmon_temp_label)
		return -EOPNOTSUPP;
	sensor = virt_temp_find_channel(channel);
	if (!sensor)
		return -ENODATA;
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

/* Caller holds virt_temp_lock. */
static int virt_temp_rebuild_hwmon(void)
{
	u32 *config;
	struct device *hwmon;
	unsigned int channel;

	config = kcalloc(virt_temp_next_channel + 1, sizeof(*config), GFP_KERNEL);
	if (!config)
		return -ENOMEM;
	/* A zero config entry terminates the channel list; visibility makes holes. */
	for (channel = 0; channel < virt_temp_next_channel; channel++)
		config[channel] = HWMON_T_INPUT | HWMON_T_LABEL;

	kfree(virt_temp_config);
	virt_temp_config = config;
	virt_temp_channel_info.config = virt_temp_config;
	hwmon = hwmon_device_register_with_info(virt_temp_misc.this_device,
						"virt_temp", NULL,
						&virt_temp_chip_info, NULL);
	if (IS_ERR(hwmon)) {
		virt_temp_hwmon = NULL;
		return PTR_ERR(hwmon);
	}
	virt_temp_hwmon = hwmon;
	return 0;
}

static int virt_temp_commit(struct virt_temp_session *session,
			    const char *namespace)
{
	struct virt_temp_sensor *sensor;
	struct virt_temp_record *record;
	bool topology_changed = false;
	int err = 0;

	if (strcmp(namespace, "disk") && strcmp(namespace, "hba"))
		return -EINVAL;
	list_for_each_entry(record, &session->records, node) {
		if (!virt_temp_in_namespace(record->id, namespace))
			return -EINVAL;
	}

	mutex_lock(&virt_temp_lock);
	list_for_each_entry(sensor, &virt_temp_sensors, node) {
		if (sensor->active && virt_temp_in_namespace(sensor->id, namespace) &&
		    !virt_temp_find_record(session, sensor->id))
			topology_changed = true;
	}
	list_for_each_entry(record, &session->records, node) {
		sensor = virt_temp_find_sensor(record->id);
		if (!sensor || !sensor->active || strcmp(sensor->label, record->label))
			topology_changed = true;
	}
	/* Drain every sysfs callback before changing its channel backing data. */
	if (topology_changed && virt_temp_hwmon) {
		hwmon_device_unregister(virt_temp_hwmon);
		virt_temp_hwmon = NULL;
	}

	list_for_each_entry(sensor, &virt_temp_sensors, node) {
		if (sensor->active && virt_temp_in_namespace(sensor->id, namespace) &&
		    !virt_temp_find_record(session, sensor->id)) {
			sensor->active = false;
			topology_changed = true;
		}
	}

	list_for_each_entry(record, &session->records, node) {
		sensor = virt_temp_find_sensor(record->id);
		if (!sensor) {
			if (virt_temp_next_channel >= VIRT_TEMP_MAX_CHANNELS) {
				err = -ENOSPC;
				goto out;
			}
			sensor = kzalloc(sizeof(*sensor), GFP_KERNEL);
			if (!sensor) {
				err = -ENOMEM;
				goto out;
			}
			strscpy(sensor->id, record->id, sizeof(sensor->id));
			sensor->channel = virt_temp_next_channel++;
			list_add_tail(&sensor->node, &virt_temp_sensors);
			topology_changed = true;
		}
		if (!sensor->active) {
			sensor->active = true;
			topology_changed = true;
		}
		if (strcmp(sensor->label, record->label)) {
			strscpy(sensor->label, record->label, sizeof(sensor->label));
		}
		atomic_long_set(&sensor->temperature, record->temperature);
		WRITE_ONCE(sensor->last_update, jiffies);
	}

	if ((topology_changed || !virt_temp_hwmon) &&
	    virt_temp_has_active_sensor())
		err = virt_temp_rebuild_hwmon();
out:
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
	file->private_data = session;
	return 0;
}

static ssize_t virt_temp_write(struct file *file, const char __user *user,
			       size_t count, loff_t *offset)
{
	struct virt_temp_session *session = file->private_data;
	struct virt_temp_record *record;
	char *buffer, *cursor, *id, *temperature, *label;
	long value;
	int err;

	if (!count || count > VIRT_TEMP_WRITE_SIZE)
		return -EMSGSIZE;
	if (session->committed)
		return -EPIPE;
	buffer = memdup_user_nul(user, count);
	if (IS_ERR(buffer))
		return PTR_ERR(buffer);
	cursor = strim(buffer);

	if (!strncmp(cursor, "commit\t", 7)) {
		err = virt_temp_commit(session, cursor + 7);
		if (!err)
			session->committed = true;
		kfree(buffer);
		return err ? err : count;
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
	if (virt_temp_hwmon)
		hwmon_device_unregister(virt_temp_hwmon);
	list_for_each_entry_safe(sensor, next, &virt_temp_sensors, node) {
		list_del(&sensor->node);
		kfree(sensor);
	}
	kfree(virt_temp_config);
}

module_init(virt_temp_init);
module_exit(virt_temp_exit);

MODULE_AUTHOR("François HOYEZ");
MODULE_DESCRIPTION("Dynamic multi-channel virtual hwmon temperature device");
MODULE_LICENSE("GPL");
