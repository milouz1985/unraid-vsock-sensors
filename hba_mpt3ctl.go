// SPDX-License-Identifier: GPL-3.0-or-later

// hba_mpt3ctl.go implements the read-only userspace side of the Linux mpt3sas
// management ABI. It does not contain code copied from LSIUtil, StorCLI or the
// Linux mpt3sas driver.
//
// The ABI constants, binary layouts and MPI protocol semantics used here were
// obtained from and verified against the public Linux mpt3sas interface
// definitions:
//
//   - drivers/scsi/mpt3sas/mpt3sas_ctl.h
//     MPT3IOCINFO, MPT3COMMAND and their userspace structure layouts;
//   - drivers/scsi/mpt3sas/mpt3sas_ctl.c
//     validation and execution of the MPT3COMMAND firmware passthrough;
//   - drivers/scsi/mpt3sas/mpi/mpi2.h and mpi/mpi2_cnfg.h
//     MPI CONFIG request/reply messages and configuration-page layouts;
//   - drivers/scsi/mpt3sas/mpt3sas_config.c
//     the two-step PAGE_HEADER/PAGE_READ_CURRENT access pattern;
//   - drivers/scsi/mpt3sas/mpt3sas_hwmon.c
//     IO Unit Page 7 temperature semantics, including signed values and units.
//
// Upstream sources (Linux commit 8cbaf7b1ab4dd9ced322b6ebf60b079cc3a3d8d2):
// https://github.com/torvalds/linux/tree/8cbaf7b1ab4dd9ced322b6ebf60b079cc3a3d8d2/drivers/scsi/mpt3sas
//
// The byte layouts and offsets were also validated on Linux/amd64 against an
// LSI SAS3008 using mpt3sas 54.100.00.00. The resulting IOC temperature matched
// the value reported by LSIUtil.
//
// Only MPI CONFIG PAGE_HEADER and PAGE_READ_CURRENT requests are constructed.
// No caller-provided MPI frame, data-out buffer, firmware write, reset or
// diagnostic operation is exposed. The ioctl constants and command layout
// below are deliberately limited to Linux x86-64, the only architecture Unraid
// supports. The MPT3 command is encoded at fixed byte offsets instead of using
// Go struct alignment. TestMPT3CommandABI locks that userspace ABI.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"unsafe"

	"golang.org/x/sys/unix"

	"unraid-vsock-sensors/internal/sensors"
)

const (
	mpt3ctlPath         = "/dev/mpt3ctl"
	mpt3CommandIOCTL    = uintptr(0xc0484c14)
	mpt3IOCInfoIOCTL    = uintptr(0xc05c4c11)
	mpt3IOCSlots        = 1 << 8
	mpt3FirmwareTimeout = 10
)

// MPT3COMMAND layout (96 bytes), from struct mpt3_cmd in mpt3sas_ctl.h.
// Written at fixed byte offsets because Go struct alignment would not match
// the kernel's packed ABI. Offsets not listed are zero and unused.
const (
	mpt3CommandSize          = 96
	mpt3CommandIOCOffset     = 0  // uint32 IOC number
	mpt3CommandLenOffset     = 8  // uint32 total length of data in/out buffers
	mpt3CommandTimeoutOffset = 12 // uint32 firmware timeout in seconds
	mpt3CommandReplyPtrOff   = 16 // uint64 pointer to reply buffer
	mpt3CommandDataInPtrOff  = 24 // uint64 pointer to data-in buffer
	mpt3CommandMaxReplyOff   = 48 // uint32 maximum reply size
	mpt3CommandDataInSizeOff = 52 // uint32 data-in buffer size
	mpt3CommandSGEOffset     = 64 // uint32 number of SGEs (7 = fixed, no SGE list)
	mpt3CommandRequestOffset = 68 // 28-byte MPI request frame
)

const mpt3ReplyBufferSize = 128

// MPI CONFIG function and page identifiers (mpi2_cnfg.h).
const (
	mpi2FunctionConfig        = 0x04
	mpi2ConfigPageHeader      = 0x00
	mpi2ConfigPageReadCurrent = 0x01
	mpi2PageTypeIOUnit        = 0x00
	// Keep these values aligned with the MPI2_*_PAGEVERSION definitions in
	// mpi2_cnfg.h used by the in-kernel mpt3sas CONFIG helpers.
	mpi2IOUnit7Version = 0x05
)

// IO Unit Page 7 temperature unit encoding (mpt3sas_hwmon.c).
const (
	temperatureNotPresent = 0x00
	temperatureFahrenheit = 0x01
	temperatureCelsius    = 0x02
)

const (
	mpi2ConfigReplySize   = 0x18
	mpi2ConfigReplyDWords = mpi2ConfigReplySize / 4
	mpi2IOCStatusMask     = 0x7fff
)

type mpt3Command [mpt3CommandSize]byte

func mpt3ConfigRequest(action, pageType, pageNumber, pageVersion byte, header []byte) [28]byte {
	var request [28]byte
	request[0], request[3] = action, mpi2FunctionConfig
	if header == nil {
		// Like the in-kernel mpt3sas helpers, PAGE_HEADER requests include the
		// version expected by the driver instead of relying on a zero-filled
		// PageVersion. This matters for pages whose declared version is nonzero.
		request[20], request[22], request[23] = pageVersion, pageNumber, pageType
	} else {
		// PAGE_READ_CURRENT must use the complete header returned by the
		// preceding PAGE_HEADER request, including the firmware's PageVersion
		// and PageLength.
		copy(request[20:24], header)
	}
	return request
}

func makeMPT3Command(ioc int, request [28]byte, dataSize int, replyPointer, dataInPointer uintptr) mpt3Command {
	var command mpt3Command
	binary.LittleEndian.PutUint32(command[mpt3CommandIOCOffset:], uint32(ioc))
	binary.LittleEndian.PutUint32(command[mpt3CommandLenOffset:], uint32(max(dataSize, mpt3ReplyBufferSize)))
	binary.LittleEndian.PutUint32(command[mpt3CommandTimeoutOffset:], mpt3FirmwareTimeout)
	binary.LittleEndian.PutUint64(command[mpt3CommandReplyPtrOff:], uint64(replyPointer))
	binary.LittleEndian.PutUint64(command[mpt3CommandDataInPtrOff:], uint64(dataInPointer))
	binary.LittleEndian.PutUint32(command[mpt3CommandMaxReplyOff:], mpt3ReplyBufferSize)
	binary.LittleEndian.PutUint32(command[mpt3CommandDataInSizeOff:], uint32(dataSize))
	binary.LittleEndian.PutUint32(command[mpt3CommandSGEOffset:], 7)
	copy(command[mpt3CommandRequestOffset:], request[:])
	return command
}

type mpt3Device struct{ file *os.File }

func openMPT3() (*mpt3Device, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, fmt.Errorf("%w: mpt3ctl requires linux/amd64", errHBABackendUnavailable)
	}
	file, err := os.OpenFile(mpt3ctlPath, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s does not exist", errHBABackendUnavailable, mpt3ctlPath)
		}
		return nil, fmt.Errorf("open %s: %w", mpt3ctlPath, err)
	}
	return &mpt3Device{file: file}, nil
}

func mpt3IOCTL(fd, request uintptr, argument unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, request, uintptr(argument))
	if errno != 0 {
		return errno
	}
	return nil
}

func (d *mpt3Device) iocInfo(ioc int) ([]byte, error) {
	const iocInfoSize = 92
	buffer := make([]byte, iocInfoSize)
	binary.LittleEndian.PutUint32(buffer[0:4], uint32(ioc))
	binary.LittleEndian.PutUint32(buffer[8:12], uint32(len(buffer)))
	if err := mpt3IOCTL(d.file.Fd(), mpt3IOCInfoIOCTL, unsafe.Pointer(&buffer[0])); err != nil {
		return nil, err
	}
	return buffer, nil
}

func (d *mpt3Device) command(ioc int, request [28]byte, dataSize int) ([]byte, []byte, error) {
	reply := make([]byte, mpt3ReplyBufferSize)
	data := make([]byte, dataSize)
	dataInPointer := uintptr(0)
	if dataSize > 0 {
		dataInPointer = uintptr(unsafe.Pointer(&data[0]))
	}
	command := makeMPT3Command(ioc, request, dataSize,
		uintptr(unsafe.Pointer(&reply[0])), dataInPointer)
	err := mpt3IOCTL(d.file.Fd(), mpt3CommandIOCTL, unsafe.Pointer(&command[0]))
	// The command encodes reply and data addresses as integer ABI fields. Keep
	// them reachable until the ioctl returns, so the GC cannot reclaim the
	// backing allocations before the kernel has finished with them.
	runtime.KeepAlive(command)
	runtime.KeepAlive(reply)
	runtime.KeepAlive(data)
	if err != nil {
		return nil, nil, err
	}
	return reply, data, nil
}

// validateMPT3ConfigReply checks that an MPI CONFIG reply matches the action,
// page type, and page number the driver asked for. The kernel does not return
// a byte count, so this also rejects a zero-filled reply buffer via the MPI
// MsgLength field (in 32-bit words) at offset 2.
func validateMPT3ConfigReply(reply []byte, action, pageType, pageNumber byte) error {
	if len(reply) < mpi2ConfigReplySize {
		return fmt.Errorf("short MPI CONFIG reply: got %d bytes, need %d", len(reply), mpi2ConfigReplySize)
	}
	messageBytes := int(reply[0x02]) * 4
	if messageBytes < mpi2ConfigReplySize || messageBytes > len(reply) {
		return fmt.Errorf("invalid MPI CONFIG MsgLength: %d bytes", messageBytes)
	}
	if reply[0x03] != mpi2FunctionConfig {
		return fmt.Errorf("unexpected MPI function 0x%02x", reply[0x03])
	}
	if reply[0x00] != action {
		return fmt.Errorf("unexpected MPI CONFIG action 0x%02x, want 0x%02x", reply[0x00], action)
	}
	status := binary.LittleEndian.Uint16(reply[0x0e:0x10]) & mpi2IOCStatusMask
	if status != 0 {
		logInfo := binary.LittleEndian.Uint32(reply[0x10:0x14])
		return fmt.Errorf("MPI CONFIG returned IOCStatus 0x%04x, IOCLogInfo 0x%08x", status, logInfo)
	}
	if reply[0x17]&0x0f != pageType&0x0f {
		return fmt.Errorf("unexpected MPI CONFIG page type 0x%02x, want 0x%02x", reply[0x17]&0x0f, pageType&0x0f)
	}
	if reply[0x16] != pageNumber {
		return fmt.Errorf("unexpected MPI CONFIG page number %d, want %d", reply[0x16], pageNumber)
	}
	return nil
}

// readConfigPage performs the two-step MPI CONFIG access pattern from
// mpt3sas_config.c: PAGE_HEADER to get the page's version and length, then
// PAGE_READ_CURRENT to read the page using that header.
func (d *mpt3Device) readConfigPage(ctx context.Context, ioc int, pageType, pageNumber, pageVersion byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reply, _, err := d.command(ioc, mpt3ConfigRequest(mpi2ConfigPageHeader, pageType, pageNumber, pageVersion, nil), 0)
	if err != nil {
		return nil, fmt.Errorf("CONFIG header type 0x%02x page %d: %w", pageType, pageNumber, err)
	}
	if err := validateMPT3ConfigReply(reply, mpi2ConfigPageHeader, pageType, pageNumber); err != nil {
		return nil, fmt.Errorf("CONFIG header type 0x%02x page %d: %w", pageType, pageNumber, err)
	}
	header := reply[0x14:0x18]
	pageSize := int(header[1]) * 4
	if pageSize == 0 {
		return nil, fmt.Errorf("CONFIG type 0x%02x page %d has zero length", pageType, pageNumber)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reply, page, err := d.command(ioc, mpt3ConfigRequest(mpi2ConfigPageReadCurrent, pageType, pageNumber, pageVersion, header), pageSize)
	if err != nil {
		return nil, fmt.Errorf("CONFIG read type 0x%02x page %d: %w", pageType, pageNumber, err)
	}
	if err := validateMPT3ConfigReply(reply, mpi2ConfigPageReadCurrent, pageType, pageNumber); err != nil {
		return nil, fmt.Errorf("CONFIG read type 0x%02x page %d: %w", pageType, pageNumber, err)
	}
	return page, nil
}

func parseMPT3PCIAddress(info []byte) string {
	const minimum = 92
	if len(info) < minimum {
		return ""
	}
	pci, segment := binary.LittleEndian.Uint32(info[84:88]), binary.LittleEndian.Uint32(info[88:92])
	device, function, bus := pci&0x1f, (pci>>5)&0x07, pci>>8
	if segment > 0xffff || bus > 0xff {
		return ""
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", segment, bus, device, function)
}

func parseMPT3Temperature(page []byte) (float64, error) {
	const minimum = 0x13
	if len(page) < minimum {
		return 0, errors.New("IO Unit Page 7 is shorter than the IOC temperature fields")
	}
	raw := int16(binary.LittleEndian.Uint16(page[0x10:0x12]))
	var temperature float64
	switch page[0x12] {
	case temperatureCelsius:
		temperature = float64(raw)
	case temperatureFahrenheit:
		temperature = (float64(raw) - 32) * 5 / 9
	case temperatureNotPresent:
		return 0, errors.New("IOC temperature sensor is not present")
	default:
		return 0, fmt.Errorf("unsupported IOC temperature unit 0x%02x", page[0x12])
	}
	return temperature, nil
}

type mpt3Reader struct{ sysfsRoot string }

func newMPT3Reader() *mpt3Reader {
	return &mpt3Reader{sysfsRoot: defaultSCSIHostRoot}
}

type discoveredController struct {
	ioc      int
	metadata hbaMetadata
}

// collect scans the IOC range, associates each controller with its sysfs
// identity, reads its temperature page, and returns readings sorted by ID.
// A collection that ends with zero readings returns errNoHBA.
func (r *mpt3Reader) collect(ctx context.Context) ([]sensors.HBA, error) {
	device, err := openMPT3()
	if err != nil {
		return nil, err
	}
	defer device.file.Close()

	controllers, err := r.discoverControllers(ctx, device)
	if err != nil {
		return nil, err
	}

	readings := make([]sensors.HBA, 0, len(controllers))
	for _, c := range controllers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reading, err := r.readController(ctx, device, c)
		if err != nil {
			return nil, err
		}
		readings = append(readings, reading)
	}

	if len(readings) == 0 {
		return nil, errNoHBA
	}
	sort.Slice(readings, func(i, j int) bool { return readings[i].ID < readings[j].ID })
	return readings, nil
}

func (r *mpt3Reader) discoverControllers(ctx context.Context, device *mpt3Device) ([]discoveredController, error) {
	identities, err := discoverSysfsHBAs(ctx, r.sysfsRoot)
	if err != nil {
		return nil, fmt.Errorf("read sysfs HBA identities: %w", err)
	}
	return matchMPT3Controllers(ctx, identities, device.iocInfo)
}

func matchMPT3Controllers(ctx context.Context, identities map[string]hbaMetadata, iocInfo func(int) ([]byte, error)) ([]discoveredController, error) {
	wanted := 0
	for _, identity := range identities {
		if identity.driver == "mpt3sas" {
			wanted++
		}
	}
	if wanted == 0 {
		return nil, errNoHBA
	}

	matched := make(map[string]int)
	controllers := make([]discoveredController, 0, wanted)
	for ioc := 0; ioc < mpt3IOCSlots && len(controllers) < wanted; ioc++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := iocInfo(ioc)
		if err != nil {
			// ENODEV/EINVAL/ENXIO are the expected "no controller at this IOC"
			// errors; anything else is a real transport failure.
			if errors.Is(err, unix.ENODEV) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENXIO) {
				continue
			}
			return nil, fmt.Errorf("mpt3ctl IOC %d discovery: %w", ioc, err)
		}
		pci := parseMPT3PCIAddress(info)
		identity, found := identities[pci]
		if !found || identity.driver != "mpt3sas" {
			return nil, fmt.Errorf("mpt3ctl IOC %d at %s is missing from sysfs HBA inventory", ioc, pci)
		}
		if previous, duplicate := matched[identity.id]; duplicate {
			return nil, fmt.Errorf("mpt3ctl IOCs %d and %d have duplicate identity %q", previous, ioc, identity.id)
		}
		matched[identity.id] = ioc
		controllers = append(controllers, discoveredController{ioc: ioc, metadata: identity})
	}
	if len(controllers) != wanted {
		return nil, fmt.Errorf("mpt3ctl matched %d of %d mpt3sas controllers from sysfs", len(controllers), wanted)
	}
	return controllers, nil
}

func (r *mpt3Reader) readController(ctx context.Context, device *mpt3Device, c discoveredController) (sensors.HBA, error) {
	page, err := device.readConfigPage(ctx, c.ioc, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version)
	if err != nil {
		return sensors.HBA{}, fmt.Errorf("mpt3ctl IOC %d temperature: %w", c.ioc, err)
	}
	temperature, err := parseMPT3Temperature(page)
	if err != nil {
		return sensors.HBA{}, fmt.Errorf("mpt3ctl IOC %d temperature: %w", c.ioc, err)
	}
	return sensors.HBA{
		ID:         c.metadata.id,
		Model:      c.metadata.model,
		PCIAddress: c.metadata.pciAddress,
		Temp:       temperature,
	}, nil
}
