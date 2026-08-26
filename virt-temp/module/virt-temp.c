// SPDX-License-Identifier: GPL-2.0-only

#include <linux/hwmon.h>
#include <linux/jiffies.h>
#include <linux/module.h>
#include <linux/platform_device.h>

#define VIRT_TEMP_FAILSAFE_MILLIC 100000L
#define VIRT_TEMP_MAX_MILLIC 150000L

static unsigned int stale_timeout = 10;
module_param(stale_timeout, uint, 0644);
MODULE_PARM_DESC(stale_timeout,
		 "Seconds without an update before reporting 100 degrees Celsius");

/*
 * Sysfs read and write callbacks may run concurrently. atomic_long_t keeps the
 * milli-degree value indivisible, so readers always observe either the old or
 * the new complete value without requiring a lock in this single-value state.
 */
static atomic_long_t temperature = ATOMIC_LONG_INIT(VIRT_TEMP_FAILSAFE_MILLIC);
static unsigned long last_update;
static struct platform_device *virt_temp_device;

static bool virt_temp_is_stale(void)
{
	unsigned int timeout = max(stale_timeout, 1U);

	/*
	 * Cast before multiplying so the timeout is calculated in the same
	 * unsigned-long jiffies domain as last_update, without first overflowing
	 * the narrower unsigned-int type used by the module parameter. READ_ONCE
	 * forces one actual load because the sysfs write callback may update
	 * last_update concurrently; a word-sized snapshot is sufficient here and
	 * does not require a lock.
	 */
	return time_after(jiffies,
			  READ_ONCE(last_update) + (unsigned long)timeout * HZ);
}

static umode_t virt_temp_is_visible(const void *data,
				    enum hwmon_sensor_types type,
				    u32 attr, int channel)
{
	if (type != hwmon_temp || channel != 0)
		return 0;

	switch (attr) {
	case hwmon_temp_input:
		return 0644;
	case hwmon_temp_label:
		return 0444;
	default:
		return 0;
	}
}

static int virt_temp_read(struct device *dev, enum hwmon_sensor_types type,
			  u32 attr, int channel, long *value)
{
	if (type != hwmon_temp || attr != hwmon_temp_input || channel != 0)
		return -EOPNOTSUPP;

	*value = virt_temp_is_stale() ? VIRT_TEMP_FAILSAFE_MILLIC
					   : atomic_long_read(&temperature);
	return 0;
}

static int virt_temp_read_string(struct device *dev,
				 enum hwmon_sensor_types type,
				 u32 attr, int channel, const char **str)
{
	if (type != hwmon_temp || attr != hwmon_temp_label || channel != 0)
		return -EOPNOTSUPP;

	*str = "HDD maximum";
	return 0;
}

static int virt_temp_write(struct device *dev, enum hwmon_sensor_types type,
			   u32 attr, int channel, long value)
{
	if (type != hwmon_temp || attr != hwmon_temp_input || channel != 0)
		return -EOPNOTSUPP;
	if (value < 0 || value > VIRT_TEMP_MAX_MILLIC)
		return -ERANGE;

	atomic_long_set(&temperature, value);
	WRITE_ONCE(last_update, jiffies);
	return 0;
}

static const struct hwmon_ops virt_temp_ops = {
	.is_visible = virt_temp_is_visible,
	.read = virt_temp_read,
	.read_string = virt_temp_read_string,
	.write = virt_temp_write,
};

static const struct hwmon_channel_info * const virt_temp_info[] = {
	HWMON_CHANNEL_INFO(temp, HWMON_T_INPUT | HWMON_T_LABEL),
	NULL
};

static const struct hwmon_chip_info virt_temp_chip_info = {
	.ops = &virt_temp_ops,
	.info = virt_temp_info,
};

static int virt_temp_probe(struct platform_device *pdev)
{
	struct device *hwmon;

	WRITE_ONCE(last_update, jiffies);
	hwmon = devm_hwmon_device_register_with_info(&pdev->dev, "virt_temp",
						      NULL, &virt_temp_chip_info,
						      NULL);
	return PTR_ERR_OR_ZERO(hwmon);
}

static struct platform_driver virt_temp_driver = {
	.probe = virt_temp_probe,
	.driver = {
		.name = "virt-temp",
	},
};

static int __init virt_temp_init(void)
{
	int err;

	err = platform_driver_register(&virt_temp_driver);
	if (err)
		return err;

	virt_temp_device = platform_device_register_simple("virt-temp", -1,
							NULL, 0);
	if (IS_ERR(virt_temp_device)) {
		err = PTR_ERR(virt_temp_device);
		platform_driver_unregister(&virt_temp_driver);
		return err;
	}

	return 0;
}

static void __exit virt_temp_exit(void)
{
	platform_device_unregister(virt_temp_device);
	platform_driver_unregister(&virt_temp_driver);
}

module_init(virt_temp_init);
module_exit(virt_temp_exit);

MODULE_AUTHOR("François HOYEZ");
MODULE_DESCRIPTION("Writable virtual hwmon temperature sensor with a failsafe timeout");
MODULE_LICENSE("GPL");
