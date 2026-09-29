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
//     MPT3COMMAND and its userspace structure layout;
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
// Go struct alignment. TestMPT3CommandABI locks UVSS's transcription of that
// userspace structure; it does not verify the ABI of the loaded kernel. An
// incompatible mpt3_ioctl_command layout change that preserves its size remains
// undetectable here.
package main

import (
	"bytes"
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
	mpt3FirmwareTimeout = 10
)

// MPT3COMMAND buffer layout, from struct mpt3_ioctl_command in mpt3sas_ctl.h.
// The ioctl encodes the 72-byte x86-64 struct size, but mf[1] starts at offset
// 68 and data_sge_offset makes the kernel copy a 28-byte MPI request frame from
// there. The userspace allocation must therefore be 68 + 28 = 96 bytes.
// Fixed byte offsets avoid relying on Go struct alignment. Unlisted fields are
// zero and unused by MPT3COMMAND.
const (
	mpt3CommandSize          = 96
	mpt3CommandIOCOffset     = 0  // uint32 IOC number
	mpt3CommandTimeoutOffset = 12 // uint32 firmware timeout in seconds
	mpt3CommandReplyPtrOff   = 16 // uint64 pointer to reply buffer
	mpt3CommandDataInPtrOff  = 24 // uint64 pointer to data-in buffer
	mpt3CommandMaxReplyOff   = 48 // uint32 maximum reply size
	mpt3CommandDataInSizeOff = 52 // uint32 data-in buffer size
	mpt3CommandSGEOffset     = 64 // uint32 word offset to the first SGL (7 = 28 bytes)
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
	mpi2IOUnit7DWords  = 10
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

func makeMPT3Command(ioc int, request [28]byte, dataSize int, replyPointer, dataInPointer unsafe.Pointer) mpt3Command {
	var command mpt3Command
	binary.LittleEndian.PutUint32(command[mpt3CommandIOCOffset:], uint32(ioc))
	binary.LittleEndian.PutUint32(command[mpt3CommandTimeoutOffset:], mpt3FirmwareTimeout)
	binary.LittleEndian.PutUint64(command[mpt3CommandReplyPtrOff:], uint64(uintptr(replyPointer)))
	binary.LittleEndian.PutUint64(command[mpt3CommandDataInPtrOff:], uint64(uintptr(dataInPointer)))
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

func (d *mpt3Device) command(ioc int, request [28]byte, dataSize int) ([]byte, []byte, error) {
	reply := make([]byte, mpt3ReplyBufferSize)
	data := make([]byte, dataSize)
	// These pointers are encoded as integers inside command rather than passed
	// directly to the syscall. Pin their backing arrays until the ioctl returns
	// so the encoded addresses remain valid independently of GC movement.
	var pinner runtime.Pinner
	pinner.Pin(&reply[0])
	defer pinner.Unpin()

	var dataInPointer unsafe.Pointer
	if dataSize > 0 {
		pinner.Pin(&data[0])
		dataInPointer = unsafe.Pointer(&data[0])
	}
	command := makeMPT3Command(ioc, request, dataSize,
		unsafe.Pointer(&reply[0]), dataInPointer)
	err := mpt3IOCTL(d.file.Fd(), mpt3CommandIOCTL, unsafe.Pointer(&command[0]))
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
	if reply[0x02] != mpi2ConfigReplyDWords {
		return fmt.Errorf("invalid MPI CONFIG MsgLength: got %d DWORDs, want %d", reply[0x02], mpi2ConfigReplyDWords)
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

func validateMPT3ConfigPageHeader(header []byte, pageType, pageNumber, pageVersion, pageDWords byte) error {
	if len(header) < 4 {
		return fmt.Errorf("short MPI CONFIG page header: got %d bytes, need 4", len(header))
	}
	if header[0] != pageVersion {
		return fmt.Errorf("unexpected MPI CONFIG PageVersion 0x%02x, want 0x%02x", header[0], pageVersion)
	}
	if header[1] != pageDWords {
		return fmt.Errorf("unexpected MPI CONFIG PageLength %d DWORDs, want %d", header[1], pageDWords)
	}
	if header[2] != pageNumber {
		return fmt.Errorf("unexpected MPI CONFIG page number %d, want %d", header[2], pageNumber)
	}
	if header[3]&0x0f != pageType&0x0f {
		return fmt.Errorf("unexpected MPI CONFIG page type 0x%02x, want 0x%02x", header[3]&0x0f, pageType&0x0f)
	}
	return nil
}

func validateMPT3ConfigPageData(page []byte, header [4]byte) error {
	if len(page) < len(header) {
		return fmt.Errorf("short MPI CONFIG page: got %d bytes, need at least %d", len(page), len(header))
	}
	if !bytes.Equal(page[:len(header)], header[:]) {
		return fmt.Errorf("MPI CONFIG page header %x does not match PAGE_HEADER response %x", page[:len(header)], header)
	}
	return nil
}

// readIOUnitPage7 performs the two-step MPI CONFIG access pattern from
// mpt3sas_config.c: PAGE_HEADER to get the page's version and length, then
// PAGE_READ_CURRENT to read the page using that header.
func (d *mpt3Device) readIOUnitPage7(ctx context.Context, ioc int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reply, _, err := d.command(ioc, mpt3ConfigRequest(mpi2ConfigPageHeader, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, nil), 0)
	if err != nil {
		return nil, fmt.Errorf("CONFIG header IO Unit Page 7: %w", err)
	}
	if err := validateMPT3ConfigReply(reply, mpi2ConfigPageHeader, mpi2PageTypeIOUnit, 7); err != nil {
		return nil, fmt.Errorf("CONFIG header IO Unit Page 7: %w", err)
	}
	var header [4]byte
	copy(header[:], reply[0x14:0x18])
	if err := validateMPT3ConfigPageHeader(header[:], mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, mpi2IOUnit7DWords); err != nil {
		return nil, fmt.Errorf("CONFIG header IO Unit Page 7: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reply, page, err := d.command(ioc, mpt3ConfigRequest(mpi2ConfigPageReadCurrent, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version, header[:]), int(mpi2IOUnit7DWords)*4)
	if err != nil {
		return nil, fmt.Errorf("CONFIG read IO Unit Page 7: %w", err)
	}
	if err := validateMPT3ConfigReply(reply, mpi2ConfigPageReadCurrent, mpi2PageTypeIOUnit, 7); err != nil {
		return nil, fmt.Errorf("CONFIG read IO Unit Page 7: %w", err)
	}
	if !bytes.Equal(reply[0x14:0x18], header[:]) {
		return nil, fmt.Errorf("CONFIG read IO Unit Page 7 returned header %x, want %x", reply[0x14:0x18], header)
	}
	if err := validateMPT3ConfigPageData(page, header); err != nil {
		return nil, fmt.Errorf("CONFIG read IO Unit Page 7: %w", err)
	}
	return page, nil
}

func parseMPT3Temperatures(page []byte) (hbaTemperatures, error) {
	const minimum = 0x17
	if len(page) < minimum {
		return hbaTemperatures{}, errors.New("IO Unit Page 7 is shorter than the temperature fields")
	}
	return hbaTemperatures{
		ioc:   decodeMPT3Temperature(page[0x10:0x12], page[0x12]),
		board: decodeMPT3Temperature(page[0x14:0x16], page[0x16]),
	}, nil
}

func decodeMPT3Temperature(rawBytes []byte, units byte) *float64 {
	raw := int16(binary.LittleEndian.Uint16(rawBytes))
	var temperature float64
	switch units {
	case temperatureCelsius:
		temperature = float64(raw)
	case temperatureFahrenheit:
		temperature = (float64(raw) - 32) * 5 / 9
	default:
		return nil
	}
	return &temperature
}

type mpt3Reader struct{ sysfsRoot string }

func newMPT3Reader() *mpt3Reader {
	return &mpt3Reader{sysfsRoot: defaultSCSIHostRoot}
}

type discoveredController struct {
	ioc      int
	metadata hbaMetadata
}

// collect discovers IOC numbers and identities from sysfs, reads each
// controller's temperature page, and returns readings sorted by ID. A
// controller without a supported temperature probe is not exposed.
func (r *mpt3Reader) collect(ctx context.Context) ([]sensors.HBA, error) {
	device, err := openMPT3()
	if err != nil {
		return nil, err
	}
	defer device.file.Close()

	controllers, err := r.discoverControllers(ctx)
	if err != nil {
		return nil, err
	}

	readings := make([]sensors.HBA, 0, len(controllers))
	for _, c := range controllers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reading, available, err := r.readController(ctx, device, c)
		if err != nil {
			return nil, err
		}
		if available {
			readings = append(readings, reading)
		}
	}
	sort.Slice(readings, func(i, j int) bool { return readings[i].ID < readings[j].ID })
	return readings, nil
}

func (r *mpt3Reader) discoverControllers(ctx context.Context) ([]discoveredController, error) {
	inventory, err := discoverSysfsHBAs(ctx, r.sysfsRoot)
	if err != nil {
		return nil, fmt.Errorf("read sysfs HBA identities: %w", err)
	}
	if len(inventory.mpt3ByIOC) == 0 {
		return nil, errNoHBA
	}

	controllers := make([]discoveredController, 0, len(inventory.mpt3ByIOC))
	for ioc, metadata := range inventory.mpt3ByIOC {
		controllers = append(controllers, discoveredController{ioc: ioc, metadata: metadata})
	}
	sort.Slice(controllers, func(i, j int) bool { return controllers[i].ioc < controllers[j].ioc })
	return controllers, nil
}

func (r *mpt3Reader) readController(ctx context.Context, device *mpt3Device, c discoveredController) (sensors.HBA, bool, error) {
	page, err := device.readIOUnitPage7(ctx, c.ioc)
	if err != nil {
		return sensors.HBA{}, false, fmt.Errorf("mpt3ctl IOC %d temperature: %w", c.ioc, err)
	}
	temperatures, err := parseMPT3Temperatures(page)
	if err != nil {
		return sensors.HBA{}, false, fmt.Errorf("mpt3ctl IOC %d temperature: %w", c.ioc, err)
	}
	reading, available := makeHBAReading(c.metadata, temperatures)
	return reading, available, nil
}
