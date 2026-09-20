// SPDX-License-Identifier: GPL-2.0-only

#include <linux/configfs.h>
#include <linux/ctype.h>
#include <linux/fs.h>
#include <linux/hex.h>
#include <linux/hwmon.h>
#include <linux/jiffies.h>
#include <linux/kernel.h>
#include <linux/limits.h>
#include <linux/miscdevice.h>
#include <linux/module.h>
#include <linux/mutex.h>
#include <linux/slab.h>
#include <linux/string.h>
#include <linux/uaccess.h>

#define FAILSAFE_MILLIC 100000L
#define MAX_STALE_TIMEOUT 300U
/*
 * Tests on Unraid suggest disk IDs are truncated to 79 bytes. UVSS adds the
 * "disk:" prefix, yielding at most 84 bytes plus the terminating NUL.
 */
#define ID_SIZE 85
#define LABEL_SIZE 96
#define HWMON_NAME_SIZE (sizeof("unraid_") + LABEL_SIZE - 1)
#define DEVICE_NAME_SIZE (sizeof("virt-temp-") + 2 * (ID_SIZE - 1))
#define DEVICE_NODE_SIZE (sizeof("virt-temp/") + 2 * (ID_SIZE - 1))
#define MAX_TEMP_WRITE_SIZE 32

static_assert(DEVICE_NAME_SIZE <= NAME_MAX + 1,
	      "virt_temp device name exceeds NAME_MAX");

struct virt_temp_family {
	struct config_group group;
	const char *namespace;
};

struct virt_temp_sensor {
	struct config_item item;
	struct mutex lock;
	const struct virt_temp_family *family;
	char id[ID_SIZE];
	char label[LABEL_SIZE];
	char hwmon_name[HWMON_NAME_SIZE];
	char device_name[DEVICE_NAME_SIZE];
	char device_node[DEVICE_NODE_SIZE];
	atomic_long_t temperature;
	unsigned long last_update;
	struct device *hwmon;
	struct miscdevice misc;
	bool removed;
};

static unsigned int stale_timeout = 10;
static struct configfs_subsystem virt_temp_subsystem;
static struct virt_temp_family disk_family = { .namespace = "disk" };
static struct virt_temp_family hba_family = { .namespace = "hba" };

static inline struct virt_temp_sensor *to_sensor(struct config_item *item)
{
	return item ? container_of(item, struct virt_temp_sensor, item) : NULL;
}

static inline struct virt_temp_family *to_family(struct config_group *group)
{
	return group ? container_of(group, struct virt_temp_family, group) : NULL;
}

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

static void make_device_names(struct virt_temp_sensor *sensor)
{
	char *end;
	size_t prefix = strscpy(sensor->device_name, "virt-temp-",
				 sizeof(sensor->device_name));

	end = bin2hex(sensor->device_name + prefix, sensor->id, strlen(sensor->id));
	*end = '\0';
	scnprintf(sensor->device_node, sizeof(sensor->device_node),
		  "virt-temp/%s", sensor->device_name + prefix);
}

static void make_hwmon_name(struct virt_temp_sensor *sensor)
{
	const char *label_end = strstr(sensor->label, " (");
	const char *input;
	size_t output;

	strscpy(sensor->hwmon_name, "unraid_", sizeof(sensor->hwmon_name));
	output = strlen(sensor->hwmon_name);
	if (!label_end)
		label_end = sensor->label + strlen(sensor->label);

	for (input = sensor->label;
	     input < label_end && output + 1 < sizeof(sensor->hwmon_name);
	     input++) {
		unsigned char character = *input;

		if (isalnum(character)) {
			sensor->hwmon_name[output++] = tolower(character);
		} else if (sensor->hwmon_name[output - 1] != '_') {
			sensor->hwmon_name[output++] = '_';
		}
	}

	if (output > strlen("unraid_") &&
	    sensor->hwmon_name[output - 1] == '_')
		output--;
	if (output == strlen("unraid_"))
		strscpy(sensor->hwmon_name + output, "sensor",
			sizeof(sensor->hwmon_name) - output);
	else
		sensor->hwmon_name[output] = '\0';
}

static void unregister_hwmon(struct virt_temp_sensor *sensor)
{
	if (!sensor->hwmon)
		return;
	hwmon_device_unregister(sensor->hwmon);
	sensor->hwmon = NULL;
}

static int register_hwmon(struct virt_temp_sensor *sensor)
{
	if (!sensor->label[0])
		return -EINVAL;
	make_hwmon_name(sensor);
	sensor->hwmon = hwmon_device_register_with_info(
		sensor->misc.this_device, sensor->hwmon_name, sensor,
		&temp_chip_info, NULL);
	if (IS_ERR(sensor->hwmon)) {
		int err = PTR_ERR(sensor->hwmon);

		sensor->hwmon = NULL;
		return err;
	}
	return 0;
}

static int temperature_open(struct inode *inode, struct file *file)
{
	struct miscdevice *misc = file->private_data;
	struct virt_temp_sensor *sensor =
		container_of(misc, struct virt_temp_sensor, misc);

	config_item_get(&sensor->item);
	mutex_lock(&sensor->lock);
	if (sensor->removed) {
		mutex_unlock(&sensor->lock);
		config_item_put(&sensor->item);
		return -ENODEV;
	}
	file->private_data = sensor;
	mutex_unlock(&sensor->lock);
	return 0;
}

static ssize_t temperature_write(struct file *file, const char __user *user,
				 size_t count, loff_t *offset)
{
	struct virt_temp_sensor *sensor = file->private_data;
	long value;
	int err;

	if (!count || count > MAX_TEMP_WRITE_SIZE)
		return -EMSGSIZE;
	err = kstrtol_from_user(user, count, 10, &value);
	if (err)
		return err;

	/* Serialize the update against rmdir so a successful write is live. */
	mutex_lock(&sensor->lock);
	if (sensor->removed) {
		err = -ENODEV;
	} else {
		atomic_long_set(&sensor->temperature, value);
		smp_store_release(&sensor->last_update, jiffies);
		err = count;
	}
	mutex_unlock(&sensor->lock);
	return err;
}

static int temperature_release(struct inode *inode, struct file *file)
{
	struct virt_temp_sensor *sensor = file->private_data;

	config_item_put(&sensor->item);
	return 0;
}

static const struct file_operations temperature_fops = {
	.owner = THIS_MODULE,
	.open = temperature_open,
	.write = temperature_write,
	.release = temperature_release,
};

static ssize_t sensor_label_show(struct config_item *item, char *page)
{
	struct virt_temp_sensor *sensor = to_sensor(item);
	ssize_t length;

	mutex_lock(&sensor->lock);
	length = sysfs_emit(page, "%s\n", sensor->label);
	mutex_unlock(&sensor->lock);
	return length;
}

static ssize_t sensor_label_store(struct config_item *item, const char *page,
				  size_t count)
{
	struct virt_temp_sensor *sensor = to_sensor(item);
	char label[LABEL_SIZE + 1];
	char previous[LABEL_SIZE];
	char *trimmed;
	int rollback_err;
	int err = 0;

	if (!count || count >= sizeof(label))
		return -EINVAL;
	memcpy(label, page, count);
	label[count] = '\0';
	if (memchr(label, '\0', count))
		return -EINVAL;
	trimmed = strim(label);
	if (!*trimmed || strlen(trimmed) >= LABEL_SIZE ||
	    strpbrk(trimmed, "\t\r\n"))
		return -EINVAL;

	mutex_lock(&sensor->lock);
	if (sensor->removed) {
		err = -ENODEV;
		goto out;
	}
	if (!strcmp(sensor->label, trimmed)) {
		if (!sensor->hwmon) {
			err = register_hwmon(sensor);
			if (err)
				goto out;
		}
		err = count;
		goto out;
	}
	strscpy(previous, sensor->label, sizeof(previous));
	/*
	 * read_label() returns this storage without taking the lock. Keep hwmon
	 * unregistered while the label changes so readers only see stable data.
	 */
	unregister_hwmon(sensor);
	strscpy(sensor->label, trimmed, sizeof(sensor->label));
	err = register_hwmon(sensor);
	if (!err) {
		err = count;
		goto out;
	}
	strscpy(sensor->label, previous, sizeof(sensor->label));
	if (previous[0]) {
		rollback_err = register_hwmon(sensor);
		if (rollback_err)
			pr_err("failed to restore hwmon sensor %s after label update: %d\n",
			       sensor->device_name, rollback_err);
	}
out:
	mutex_unlock(&sensor->lock);
	return err;
}

CONFIGFS_ATTR(sensor_, label);

static struct configfs_attribute *sensor_attrs[] = {
	&sensor_attr_label,
	NULL,
};

static void sensor_release(struct config_item *item)
{
	struct virt_temp_sensor *sensor = to_sensor(item);

	mutex_destroy(&sensor->lock);
	kfree(sensor);
}

static struct configfs_item_operations sensor_item_ops = {
	.release = sensor_release,
};

static const struct config_item_type sensor_type = {
	.ct_item_ops = &sensor_item_ops,
	.ct_attrs = sensor_attrs,
	.ct_owner = THIS_MODULE,
};

static int decode_sensor_id(struct virt_temp_sensor *sensor, const char *name)
{
	size_t namespace_length = strlen(sensor->family->namespace);
	size_t hex_length = strlen(name);
	size_t suffix_length;
	char *suffix;
	int err;

	if (!hex_length || (hex_length & 1))
		return -EINVAL;
	suffix_length = hex_length / 2;
	if (!suffix_length || namespace_length + 1 + suffix_length >= ID_SIZE)
		return -ENAMETOOLONG;
	suffix = sensor->id + namespace_length + 1;
	err = hex2bin((u8 *)suffix, name, suffix_length);
	if (err)
		return -EINVAL;
	if (memchr(suffix, '\0', suffix_length))
		return -EINVAL;
	memcpy(sensor->id, sensor->family->namespace, namespace_length);
	sensor->id[namespace_length] = ':';
	suffix[suffix_length] = '\0';
	return 0;
}

static void unregister_sensor(struct virt_temp_sensor *sensor)
{
	mutex_lock(&sensor->lock);
	sensor->removed = true;
	unregister_hwmon(sensor);
	mutex_unlock(&sensor->lock);
	misc_deregister(&sensor->misc);
}

static struct config_item *make_sensor(struct config_group *group,
				       const char *name)
{
	struct virt_temp_family *family = to_family(group);
	struct virt_temp_sensor *sensor;
	int err;

	sensor = kzalloc(sizeof(*sensor), GFP_KERNEL);
	if (!sensor)
		return ERR_PTR(-ENOMEM);
	mutex_init(&sensor->lock);
	sensor->family = family;
	err = decode_sensor_id(sensor, name);
	if (err)
		goto fail;
	config_item_init_type_name(&sensor->item, name, &sensor_type);
	atomic_long_set(&sensor->temperature, FAILSAFE_MILLIC);
	smp_store_release(&sensor->last_update, jiffies);

	make_device_names(sensor);
	sensor->misc.minor = MISC_DYNAMIC_MINOR;
	sensor->misc.name = sensor->device_name;
	sensor->misc.fops = &temperature_fops;
	sensor->misc.nodename = sensor->device_node;
	sensor->misc.mode = 0200;
	err = misc_register(&sensor->misc);
	if (err)
		goto fail_item;
	return &sensor->item;

fail_item:
	config_item_put(&sensor->item);
	return ERR_PTR(err);
fail:
	mutex_destroy(&sensor->lock);
	kfree(sensor);
	return ERR_PTR(err);
}

static void drop_sensor(struct config_group *group, struct config_item *item)
{
	struct virt_temp_sensor *sensor = to_sensor(item);

	unregister_sensor(sensor);
	config_item_put(item);
}

static struct configfs_group_operations family_group_ops = {
	.make_item = make_sensor,
	.drop_item = drop_sensor,
};

static const struct config_item_type family_type = {
	.ct_group_ops = &family_group_ops,
	.ct_owner = THIS_MODULE,
};

static const struct config_item_type root_type = {
	.ct_owner = THIS_MODULE,
};

static int __init virt_temp_init(void)
{
	int err;

	config_group_init_type_name(&virt_temp_subsystem.su_group, "virt_temp",
				    &root_type);
	mutex_init(&virt_temp_subsystem.su_mutex);
	config_group_init_type_name(&disk_family.group, "disk", &family_type);
	config_group_init_type_name(&hba_family.group, "hba", &family_type);
	configfs_add_default_group(&disk_family.group,
				   &virt_temp_subsystem.su_group);
	configfs_add_default_group(&hba_family.group,
				   &virt_temp_subsystem.su_group);
	err = configfs_register_subsystem(&virt_temp_subsystem);
	if (err)
		mutex_destroy(&virt_temp_subsystem.su_mutex);
	return err;
}

static void __exit virt_temp_exit(void)
{
	configfs_unregister_subsystem(&virt_temp_subsystem);
	mutex_destroy(&virt_temp_subsystem.su_mutex);
}

module_init(virt_temp_init);
module_exit(virt_temp_exit);
MODULE_AUTHOR("François HOYEZ");
MODULE_DESCRIPTION("Stable per-sensor virtual hwmon temperature devices");
MODULE_LICENSE("GPL");
