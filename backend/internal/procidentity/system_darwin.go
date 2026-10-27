//go:build darwin

package procidentity

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// pcbPidOffset is where xsocket_n keeps so_last_pid in the records of net.inet.tcp.pcblist_n
// (xnu bsd/sys/socketvar.h, pack(4)). The layout is private to the kernel, so NewSystem verifies
// it at startup against a socket this process owns and refuses to run if it does not hold.
const pcbPidOffset = 68

const (
	xsoSocket = 0x001 // XSO_SOCKET record kind
	xsoInpcb  = 0x010 // XSO_INPCB record kind
)

// NewSystem returns the kernel-backed process table and socket lookup, after a self-test. It is
// pure Go (sysctl through golang.org/x/sys/unix, no cgo). An error means the lookup is
// unavailable on this machine and the caller must not use it.
func NewSystem() (ProcTable, PeerLookup, error) {
	procs, peer := kernelProcs{}, &kernelPeers{}
	if err := selfTest(procs, peer); err != nil {
		return nil, nil, fmt.Errorf("process lookup self-test failed: %w", err)
	}
	return procs, peer, nil
}

type kernelProcs struct{}

func (kernelProcs) Info(pid int) (Proc, error) {
	ki, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Proc{}, ErrNoProcess
	}
	if int(ki.Proc.P_pid) != pid {
		return Proc{}, ErrNoProcess
	}
	comm := ki.Proc.P_comm[:]
	n := 0
	for n < len(comm) && comm[n] != 0 {
		n++
	}
	return Proc{
		PID:   pid,
		PPID:  int(ki.Eproc.Ppid),
		Start: time.Unix(ki.Proc.P_starttime.Sec, int64(ki.Proc.P_starttime.Usec)*1000),
		UID:   ki.Eproc.Ucred.Uid,
		Comm:  string(comm[:n]),
	}, nil
}

// kernelPeers reads the TCP socket table. addrOffs are where the record keeps its foreign (0) and
// local (1) IPv4 addresses; like the pid offset they are kernel-private, so selfTest finds them on a
// loopback pair this process owns and refuses to run if it cannot.
type kernelPeers struct{ addrOffs [2]int }

var loopbackV4 = [4]byte{127, 0, 0, 1}

// recordIsLoopbackPair reports whether the INPCB record at buf[pos:pos+size] is a TCP socket whose
// ports are (clientPort local, serverPort foreign) AND whose local and foreign addresses are both
// 127.0.0.1: the connection the daemon is actually serving, not another interface's socket that
// merely reuses the port numbers.
func recordIsLoopbackPair(buf []byte, pos, size, clientPort, serverPort int, addrOffs [2]int) bool {
	if size < 20 || addrOffs[0] <= 0 || addrOffs[1] <= 0 || pos+size > len(buf) {
		return false
	}
	if int(binary.BigEndian.Uint16(buf[pos+18:pos+20])) != clientPort || int(binary.BigEndian.Uint16(buf[pos+16:pos+18])) != serverPort {
		return false
	}
	for _, off := range addrOffs {
		if off+4 > size || [4]byte(buf[pos+off:pos+off+4]) != loopbackV4 {
			return false
		}
	}
	return true
}

// OwnerPID reads the TCP socket table once and returns the owner of the socket with the given
// local (client) and remote (server) ports and loopback addresses.
func (k *kernelPeers) OwnerPID(clientPort, serverPort int) (int, error) {
	buf, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil {
		return 0, err
	}
	if len(buf) < 24 {
		return 0, errors.New("socket table too short")
	}
	pos := int(binary.LittleEndian.Uint32(buf[0:4])) // xinpgen header
	matched := false
	for pos+8 <= len(buf) {
		size := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		kind := binary.LittleEndian.Uint32(buf[pos+4 : pos+8])
		if size <= 0 || pos+size > len(buf) {
			break
		}
		switch kind {
		case xsoInpcb:
			matched = recordIsLoopbackPair(buf, pos, size, clientPort, serverPort, k.addrOffs)
		case xsoSocket:
			if matched && size >= pcbPidOffset+4 {
				if pid := int(binary.LittleEndian.Uint32(buf[pos+pcbPidOffset : pos+pcbPidOffset+4])); pid > 0 {
					return pid, nil
				}
				return 0, errors.New("socket has no owner")
			}
			matched = false
		}
		pos += (size + 7) &^ 7 // records are 8-byte aligned
	}
	return 0, errors.New("no socket with those ports and addresses")
}

// calibrateAddresses finds where an INPCB record keeps its local and foreign IPv4 addresses, using
// the connection this process holds: both are 127.0.0.1, so exactly two aligned offsets hold that
// value.
func (k *kernelPeers) calibrateAddresses(clientPort, serverPort int) error {
	buf, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil || len(buf) < 24 {
		return errors.New("socket table unreadable")
	}
	pos := int(binary.LittleEndian.Uint32(buf[0:4]))
	for pos+8 <= len(buf) {
		size := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		kind := binary.LittleEndian.Uint32(buf[pos+4 : pos+8])
		if size <= 0 || pos+size > len(buf) {
			break
		}
		if kind == xsoInpcb && size >= 20 &&
			int(binary.BigEndian.Uint16(buf[pos+18:pos+20])) == clientPort && int(binary.BigEndian.Uint16(buf[pos+16:pos+18])) == serverPort {
			var offs []int
			for off := 20; off+4 <= size; off += 4 {
				if [4]byte(buf[pos+off:pos+off+4]) == loopbackV4 {
					offs = append(offs, off)
				}
			}
			if len(offs) != 2 {
				return fmt.Errorf("found %d loopback address fields in the socket record, want 2", len(offs))
			}
			k.addrOffs = [2]int{offs[0], offs[1]}
			return nil
		}
		pos += (size + 7) &^ 7
	}
	return errors.New("no socket record for the calibration connection")
}

// selfTest opens a loopback connection inside this process and requires the lookup to find this
// very process as the owner of the client end, and the process table to describe it correctly.
func selfTest(procs ProcTable, peer *kernelPeers) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	serverAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return errors.New("not a TCP listener")
	}
	serverPort := serverAddr.Port
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	server, err := ln.Accept()
	if err != nil {
		return err
	}
	defer func() { _ = server.Close() }()
	clientAddr, ok := server.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return errors.New("not a TCP connection")
	}
	clientPort := clientAddr.Port

	if err := peer.calibrateAddresses(clientPort, serverPort); err != nil {
		return err
	}
	pid, err := peer.OwnerPID(clientPort, serverPort)
	if err != nil {
		return err
	}
	if pid != os.Getpid() {
		return fmt.Errorf("socket owner field does not hold this process (offset %d)", pcbPidOffset)
	}
	info, err := procs.Info(pid)
	if err != nil {
		return err
	}
	if info.PPID != os.Getppid() || info.Start.IsZero() || int(info.UID) != os.Getuid() {
		return errors.New("process table does not describe this process")
	}
	return nil
}

// All implements ProcInspector.
func (kernelProcs) All() ([]Proc, error) {
	list, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(list))
	for i := range list {
		ki := &list[i]
		comm := ki.Proc.P_comm[:]
		n := 0
		for n < len(comm) && comm[n] != 0 {
			n++
		}
		out = append(out, Proc{
			PID: int(ki.Proc.P_pid), PPID: int(ki.Eproc.Ppid),
			Start: time.Unix(ki.Proc.P_starttime.Sec, int64(ki.Proc.P_starttime.Usec)*1000),
			UID:   ki.Eproc.Ucred.Uid, Comm: string(comm[:n]),
		})
	}
	return out, nil
}

// Args implements ProcInspector: the process's argv from kern.procargs2 (argc, the executable
// path, padding, then argc NUL-terminated arguments; the environment that follows is ignored).
func (kernelProcs) Args(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 4 {
		return nil, ErrNoProcess
	}
	argc := int(binary.LittleEndian.Uint32(raw[0:4]))
	rest := raw[4:]
	end := 0
	for end < len(rest) && rest[end] != 0 { // the executable path
		end++
	}
	for end < len(rest) && rest[end] == 0 { // padding
		end++
	}
	rest = rest[end:]
	args := make([]string, 0, argc)
	for len(args) < argc && len(rest) > 0 {
		n := 0
		for n < len(rest) && rest[n] != 0 {
			n++
		}
		args = append(args, string(rest[:n]))
		if n >= len(rest) {
			break
		}
		rest = rest[n+1:]
	}
	if len(args) != argc {
		return nil, errors.New("process arguments could not be read")
	}
	return args, nil
}

// OwnerOfListener implements ListenerLookup: the owner of the TCP socket listening on
// 127.0.0.1:port (local port = port, no foreign port).
func (k *kernelPeers) OwnerOfListener(port int) (int, error) {
	buf, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil || len(buf) < 24 {
		return 0, errors.New("socket table unreadable")
	}
	pos := int(binary.LittleEndian.Uint32(buf[0:4]))
	matched := false
	for pos+8 <= len(buf) {
		size := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		kind := binary.LittleEndian.Uint32(buf[pos+4 : pos+8])
		if size <= 0 || pos+size > len(buf) {
			break
		}
		switch kind {
		case xsoInpcb:
			matched = size >= 20 && k.addrOffs[1] > 0 && k.addrOffs[1]+4 <= size &&
				int(binary.BigEndian.Uint16(buf[pos+18:pos+20])) == port && binary.BigEndian.Uint16(buf[pos+16:pos+18]) == 0 &&
				[4]byte(buf[pos+k.addrOffs[1]:pos+k.addrOffs[1]+4]) == loopbackV4
		case xsoSocket:
			if matched && size >= pcbPidOffset+4 {
				if pid := int(binary.LittleEndian.Uint32(buf[pos+pcbPidOffset : pos+pcbPidOffset+4])); pid > 0 {
					return pid, nil
				}
			}
			matched = false
		}
		pos += (size + 7) &^ 7
	}
	return 0, errors.New("no loopback listener on that port")
}
