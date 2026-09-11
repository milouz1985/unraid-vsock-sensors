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
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	"unraid-vsock-sensors/internal/sensors"
)

const (
	mpt3ctlPath               = "/dev/mpt3ctl"
	mpt3CommandIOCTL          = uintptr(0xc0484c14)
	mpt3IOCInfoIOCTL          = uintptr(0xc05c4c11)
	mpt3CommandSize           = 96
	mpt3ReplyPointerOffset    = 16
	mpt3DataInPointerOffset   = 24
	mpt3MaxReplyBytesOffset   = 48
	mpt3DataInSizeOffset      = 52
	mpt3SGEOffset             = 64
	mpt3RequestOffset         = 68
	mpt3ReplyBufferSize       = 128
	mpt3MaxIOC                = 31
	mpt3FirmwareTimeout       = 10
	mpi2FunctionConfig        = 0x04
	mpi2ConfigPageHeader      = 0x00
	mpi2ConfigPageReadCurrent = 0x01
	mpi2PageTypeIOUnit        = 0x00
	mpi2PageTypeManufacturing = 0x09
	// Keep these values aligned with the MPI2_*_PAGEVERSION definitions in
	// mpi2_cnfg.h used by the in-kernel mpt3sas CONFIG helpers.
	mpi2Manufacturing0Version = 0x00
	mpi2Manufacturing5Version = 0x03
	mpi2IOUnit7Version        = 0x05
	mpi2IOCStatusMask         = 0x7fff
	temperatureNotPresent     = 0x00
	temperatureFahrenheit     = 0x01
	temperatureCelsius        = 0x02
	mpi2ConfigReplySize       = 0x18
	mpi2ConfigReplyDWords     = mpi2ConfigReplySize / 4
)

type mpt3Command [mpt3CommandSize]byte

type mpt3Device struct{ file *os.File }

type mpt3Reader struct {
	// Manufacturing Page 5 is optional for a temperature collection. Remember a
	// SAS identity once observed so a transient page failure cannot rename the
	// same PCI controller to its weaker pci: fallback.
	sasAddressByPCI map[string]string
}

func newMPT3Reader() *mpt3Reader {
	return &mpt3Reader{sasAddressByPCI: make(map[string]string)}
}

func (r *mpt3Reader) stableID(pci, sasAddress string, page5Failed bool) string {
	if sasAddress = normalizeSASAddress(sasAddress); sasAddress != "" {
		if pci != "" {
			r.sasAddressByPCI[pci] = sasAddress
		}
		return hbaStableID(sasAddress, pci, "")
	}
	if page5Failed {
		if cached := r.sasAddressByPCI[pci]; cached != "" {
			return hbaStableID(cached, pci, "")
		}
	}
	return hbaStableID("", pci, "")
}

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
	buffer := make([]byte, 92)
	binary.LittleEndian.PutUint32(buffer[0:4], uint32(ioc))
	binary.LittleEndian.PutUint32(buffer[8:12], uint32(len(buffer)))
	if err := mpt3IOCTL(d.file.Fd(), mpt3IOCInfoIOCTL, unsafe.Pointer(&buffer[0])); err != nil {
		return nil, err
	}
	return buffer, nil
}

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
	binary.LittleEndian.PutUint32(command[0:4], uint32(ioc))
	binary.LittleEndian.PutUint32(command[8:12], uint32(max(dataSize, mpt3ReplyBufferSize)))
	binary.LittleEndian.PutUint32(command[12:16], mpt3FirmwareTimeout)
	binary.LittleEndian.PutUint64(command[mpt3ReplyPointerOffset:], uint64(replyPointer))
	binary.LittleEndian.PutUint64(command[mpt3DataInPointerOffset:], uint64(dataInPointer))
	binary.LittleEndian.PutUint32(command[mpt3MaxReplyBytesOffset:], mpt3ReplyBufferSize)
	binary.LittleEndian.PutUint32(command[mpt3DataInSizeOffset:], uint32(dataSize))
	binary.LittleEndian.PutUint32(command[mpt3SGEOffset:], 7)
	copy(command[mpt3RequestOffset:], request[:])
	return command
}

func (d *mpt3Device) command(ioc int, request [28]byte, dataSize int) ([]byte, []byte, error) {
	reply, data := make([]byte, mpt3ReplyBufferSize), make([]byte, dataSize)
	dataInPointer := uintptr(0)
	if dataSize > 0 {
		dataInPointer = uintptr(unsafe.Pointer(&data[0]))
	}
	command := makeMPT3Command(ioc, request, dataSize,
		uintptr(unsafe.Pointer(&reply[0])), dataInPointer)
	err := mpt3IOCTL(d.file.Fd(), mpt3CommandIOCTL, unsafe.Pointer(&command[0]))
	// The command stores userspace addresses as ABI integer fields. Keep their
	// backing allocations live until the kernel has finished the ioctl, even on
	// its error path.
	runtime.KeepAlive(command)
	runtime.KeepAlive(reply)
	runtime.KeepAlive(data)
	if err != nil {
		return nil, nil, err
	}
	return reply, data, nil
}

func validateMPT3ConfigReply(reply []byte, action, pageType, pageNumber byte) error {
	if len(reply) < mpi2ConfigReplySize {
		return fmt.Errorf("short MPI CONFIG reply: got %d bytes, need %d", len(reply), mpi2ConfigReplySize)
	}
	// The ioctl does not return a byte count, but MPI replies carry their own
	// length in 32-bit words. This also rejects a zero-filled reply buffer.
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

func (r *mpt3Reader) collect(ctx context.Context) ([]sensors.HBA, error) {
	device, err := openMPT3()
	if err != nil {
		return nil, err
	}
	defer device.file.Close()
	readings, identities := make([]sensors.HBA, 0), make(map[string]int)
	// IOC IDs may contain holes, so scan the configured range. Missing IOCINFO
	// calls do not query firmware; only discovered controllers trigger CONFIG reads.
	for ioc := 0; ioc <= mpt3MaxIOC; ioc++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := device.iocInfo(ioc)
		if err != nil {
			if errors.Is(err, unix.ENODEV) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENXIO) {
				continue
			}
			return nil, fmt.Errorf("mpt3ctl IOC %d discovery: %w", ioc, err)
		}
		pci, model, sasAddress := parseMPT3PCIAddress(info), "", ""
		if page, pageErr := device.readConfigPage(ctx, ioc, mpi2PageTypeManufacturing, 0, mpi2Manufacturing0Version); pageErr == nil {
			model = parseMPT3Model(page)
		}
		page5Failed := false
		if page, pageErr := device.readConfigPage(ctx, ioc, mpi2PageTypeManufacturing, 5, mpi2Manufacturing5Version); pageErr == nil {
			sasAddress = parseMPT3SASAddress(page)
		} else {
			page5Failed = true
		}
		id := r.stableID(pci, sasAddress, page5Failed)
		if id == "" {
			return nil, fmt.Errorf("mpt3ctl IOC %d has no stable identity", ioc)
		}
		if previous, duplicate := identities[id]; duplicate {
			return nil, fmt.Errorf("mpt3ctl IOCs %d and %d have duplicate identity %q", previous, ioc, id)
		}
		identities[id] = ioc
		page, err := device.readConfigPage(ctx, ioc, mpi2PageTypeIOUnit, 7, mpi2IOUnit7Version)
		if err != nil {
			return nil, fmt.Errorf("mpt3ctl IOC %d temperature: %w", ioc, err)
		}
		temperature, err := parseMPT3Temperature(page)
		if err != nil {
			return nil, fmt.Errorf("mpt3ctl IOC %d temperature: %w", ioc, err)
		}
		readings = append(readings, sensors.HBA{ID: id, Model: model, PCIAddress: pci, Temp: temperature})
	}
	if len(readings) == 0 {
		return nil, errNoHBA
	}
	sort.Slice(readings, func(i, j int) bool { return readings[i].ID < readings[j].ID })
	return readings, nil
}

func parseMPT3PCIAddress(info []byte) string {
	if len(info) < 92 {
		return ""
	}
	pci, segment := binary.LittleEndian.Uint32(info[84:88]), binary.LittleEndian.Uint32(info[88:92])
	device, function, bus := pci&0x1f, (pci>>5)&0x07, pci>>8
	if segment > 0xffff || bus > 0xff {
		return ""
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", segment, bus, device, function)
}

func parseMPT3Model(page []byte) string {
	if len(page) < 0x2c {
		return ""
	}
	// Manufacturing Page 0 distinguishes the controller chip from the product
	// board. Prefer BoardName (for example "INSPUR 3008IT") as the user-facing
	// model; use ChipName (for example "LSISAS3008") only when OEM firmware
	// leaves BoardName empty.
	if boardName := cleanMPT3ASCII(page[0x1c:0x2c]); boardName != "" {
		return boardName
	}
	return cleanMPT3ASCII(page[0x04:0x14])
}

func cleanMPT3ASCII(value []byte) string {
	if index := strings.IndexByte(string(value), 0); index >= 0 {
		value = value[:index]
	}
	value = []byte(strings.TrimRight(string(value), "\xff \t\r\n"))
	for _, character := range value {
		if character < 32 || character >= 127 {
			return ""
		}
	}
	return strings.TrimSpace(string(value))
}

func parseMPT3SASAddress(page []byte) string {
	if len(page) < 0x10 {
		return ""
	}
	for phy := 0; phy < int(page[4]); phy++ {
		offset := 0x10 + phy*16
		if offset+8 > len(page) {
			break
		}
		if address := binary.LittleEndian.Uint64(page[offset : offset+8]); address != 0 {
			return fmt.Sprintf("%016x", address)
		}
	}
	return ""
}

func parseMPT3Temperature(page []byte) (float64, error) {
	// Unlike the fixed reply buffer, this slice is allocated from the firmware's
	// PageLength. Reject a validly transferred but undersized/garbage Page 7
	// before accessing the IOC temperature fields.
	if len(page) < 0x13 {
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
	if temperature < 0 || temperature > 150 {
		return 0, fmt.Errorf("IOC temperature %.1f C is outside 0..150 C", temperature)
	}
	return temperature, nil
}
