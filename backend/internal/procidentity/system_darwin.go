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
	procs, peer := kernelProcs{}, kernelPeers{}
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

type kernelPeers struct{}

// OwnerPID reads the TCP socket table once and returns the owner of the socket with the given
// local (client) and remote (server) ports.
func (kernelPeers) OwnerPID(clientPort, serverPort int) (int, error) {
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
			matched = size >= 20 &&
				int(binary.BigEndian.Uint16(buf[pos+18:pos+20])) == clientPort &&
				int(binary.BigEndian.Uint16(buf[pos+16:pos+18])) == serverPort
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
	return 0, errors.New("no socket with those ports")
}

// selfTest opens a loopback connection inside this process and requires the lookup to find this
// very process as the owner of the client end, and the process table to describe it correctly.
func selfTest(procs ProcTable, peer PeerLookup) error {
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
