package ssh1

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

type Signal string

// POSIX signals as listed in RFC 4254 Section 6.10.
const (
	SIGABRT Signal = "ABRT"
	SIGALRM Signal = "ALRM"
	SIGFPE  Signal = "FPE"
	SIGHUP  Signal = "HUP"
	SIGILL  Signal = "ILL"
	SIGINT  Signal = "INT"
	SIGKILL Signal = "KILL"
	SIGPIPE Signal = "PIPE"
	SIGQUIT Signal = "QUIT"
	SIGSEGV Signal = "SEGV"
	SIGTERM Signal = "TERM"
	SIGUSR1 Signal = "USR1"
	SIGUSR2 Signal = "USR2"
)

var signals = map[Signal]int{
	SIGABRT: 6,
	SIGALRM: 14,
	SIGFPE:  8,
	SIGHUP:  1,
	SIGILL:  4,
	SIGINT:  2,
	SIGKILL: 9,
	SIGPIPE: 13,
	SIGQUIT: 3,
	SIGSEGV: 11,
	SIGTERM: 15,
}

type TerminalModes map[uint8]uint32

// POSIX terminal mode flags as listed in RFC, section Encoding of Terminal Modes.
const (
	tty_OP_END    = 0
	VINTR         = 1
	VQUIT         = 2
	VERASE        = 3
	VKILL         = 4
	VEOF          = 5
	VEOL          = 6
	VEOL2         = 7
	VSTART        = 8
	VSTOP         = 9
	VSUSP         = 10
	VDSUSP        = 11
	VREPRINT      = 12
	VWERASE       = 13
	VLNEXT        = 14
	VFLUSH        = 15
	VSWTCH        = 16
	VSTATUS       = 17
	VDISCARD      = 18
	IGNPAR        = 30
	PARMRK        = 31
	INPCK         = 32
	ISTRIP        = 33
	INLCR         = 34
	IGNCR         = 35
	ICRNL         = 36
	IUCLC         = 37
	IXON          = 38
	IXANY         = 39
	IXOFF         = 40
	IMAXBEL       = 41
	ISIG          = 50
	ICANON        = 51
	XCASE         = 52
	ECHO          = 53
	ECHOE         = 54
	ECHOK         = 55
	ECHONL        = 56
	NOFLSH        = 57
	TOSTOP        = 58
	IEXTEN        = 59
	ECHOCTL       = 60
	ECHOKE        = 61
	PENDIN        = 62
	OPOST         = 70
	OLCUC         = 71
	ONLCR         = 72
	OCRNL         = 73
	ONOCR         = 74
	ONLRET        = 75
	CS7           = 90
	CS8           = 91
	PARENB        = 92
	PARODD        = 93
	TTY_OP_ISPEED = 192
	TTY_OP_OSPEED = 193
)

// A Session represents a connection to a remote command or shell.
type Session struct {
	// conn backing this session.
	conn packetConn

	// Stdin specifies the remote process's standard input.
	// If Stdin is nil, the remote process reads from an empty
	// bytes.Buffer.
	Stdin io.Reader

	// Stdout and Stderr specify the remote process's standard
	// output and error.
	//
	// If either is nil, Run connects the corresponding file
	// descriptor to an instance of io.Discard. There is a
	// fixed amount of buffering that is shared for the two streams.
	// If either blocks it may eventually cause the remote
	// command to block.
	Stdout io.Writer
	Stderr io.Writer

	ch        Channel // the channel backing this session
	started   bool    // true once Start, Run or Shell is invoked.
	copyFuncs []func() error
	errors    chan error // one send per copyFunc

	// true if pipe method is active
	stdinpipe, stdoutpipe, stderrpipe bool

	// stdinPipeWriter is non-nil if StdinPipe has not been called
	// and Stdin was specified by the user; it is the write end of
	// a pipe connecting Session.Stdin to the stdin channel.
	stdinPipeWriter io.WriteCloser

	exitStatus chan error
}

func (s *Session) Close() error {
	return s.conn.Close()
}

func (s *Session) start() error {
	s.started = true

	type F func(*Session)
	for _, setupFd := range []F{(*Session).stdin, (*Session).stdout, (*Session).stderr} {
		setupFd(s)
	}

	s.errors = make(chan error, len(s.copyFuncs))
	for _, fn := range s.copyFuncs {
		go func(fn func() error) {
			s.errors <- fn()
		}(fn)
	}
	return nil
}

// Shell starts a shell on the remote host.
// A Session only accepts one call to Run, Start, Shell.
func (s *Session) Shell() error {
	if s.started {
		return errors.New("ssh1: session already started")
	}

	if err := s.conn.writePacket(cmsgExecShell, []byte{}); err != nil {
		return err
	}

	return s.start()
}

// Start runs cmd on the remote host. Typically, the remote
// server passes cmd to the shell for interpretation.
// A Session only accepts one call to Run, Start or Shell.
func (s *Session) Start(cmd string) error {
	if s.started {
		return errors.New("ssh1: session already started")
	}
	msg := execCmdCmsg{
		Command: cmd,
	}

	if err := s.conn.writePacket(Marshal(&msg)); err != nil {
		return err
	}

	return s.start()
}

// Run runs cmd on the remote host. Typically, the remote
// server passes cmd to the shell for interpretation.
// A Session only accepts one call to Run, Start, Shell, Output,
// or CombinedOutput.
//
// The returned error is nil if the command runs, has no problems
// copying stdin, stdout, and stderr, and exits with a zero exit
// status.
//
// If the remote server does not send an exit status, an error of type
// *ExitMissingError is returned. If the command completes
// unsuccessfully or is interrupted by a signal, the error is of type
// *ExitError. Other error types may be returned for I/O problems.
func (s *Session) Run(cmd string) error {
	err := s.Start(cmd)
	if err != nil {
		return err
	}
	return s.Wait()
}

func (s *Session) wait() error {
	wm := Waitmsg{status: -1}
	// Wait for msg channel to be closed before returning.
	var packet = make([]byte, 64)
	n, _ := s.ch.ReadStatus(packet)
	packet = packet[:n]
	var exitStatus exitstatusSmsg
	if err := Unmarshal(smsgExitstatus, packet, &exitStatus); err != nil {
		return err
	}
	wm.status = int(exitStatus.Status)
	if wm.status == 0 {
		return nil
	}
	if wm.status == -1 {
		wm.status = 128
		if _, ok := signals[Signal(wm.signal)]; ok {
			wm.status += signals[Signal(wm.signal)]
		}
	}

	return &ExitError{wm}
}

// Wait waits for the remote command to exit.
//
// The returned error is nil if the command runs, has no problems
// copying stdin, stdout, and stderr, and exits with a zero exit
// status.
//
// If the remote server does not send an exit status, an error of type
// *ExitMissingError is returned. If the command completes
// unsuccessfully or is interrupted by a signal, the error is of type
// *ExitError. Other error types may be returned for I/O problems.
func (s *Session) Wait() error {
	if !s.started {
		return errors.New("ssh1: session not started")
	}
	waitErr := <-s.exitStatus

	if s.stdinPipeWriter != nil {
		s.stdinPipeWriter.Close()
	}
	var copyError error
	/* for range s.copyFuncs {
		if err := <-s.errors; err != nil && copyError == nil {
			copyError = err
		}
	} */
	if waitErr != nil {
		return waitErr
	}
	return copyError
}

// RequestPty requests the association of a pty with the session on the remote host.
func (s *Session) RequestPty(term string, h, w int, termmodes TerminalModes) error {
	var tm []byte
	for opcode, value := range termmodes {
		tm = append(tm, opcode)
		if opcode >= 1 && opcode <= 127 {
			tm = append(tm, byte(value))
		} else {
			vb := make([]byte, 4)
			binary.BigEndian.PutUint32(vb, value)
			tm = append(tm, vb...)
		}
	}
	tm = append(tm, tty_OP_END)
	req := requestPTYCmsg{
		TermEnv:      term,
		Width:        uint32(w),
		Height:       uint32(h),
		WidthPixels:  0,
		HeightPixels: 0,
		TTYModes:     tm,
	}
	if err := s.conn.writePacket(Marshal(&req)); err != nil {
		return err
	}
	return nil
}

// WindowChange informs the remote host about a terminal window dimension change to h rows and w columns.
func (s *Session) WindowChange(h, w int) error {
	req := windowSizeCmsg{
		Width:        uint32(w),
		Height:       uint32(h),
		WidthPixels:  0,
		HeightPixels: 0,
	}
	if err := s.conn.writePacket(Marshal(&req)); err != nil {
		return err
	}
	return nil
}

func (s *Session) stdin() {
	if s.stdinpipe {
		return
	}
	var stdin io.Reader
	if s.Stdin == nil {
		stdin = new(bytes.Buffer)
	} else {
		r, w := io.Pipe()
		go func() {
			_, err := io.Copy(w, s.Stdin)
			w.CloseWithError(err)
		}()
		stdin, s.stdinPipeWriter = r, w
	}
	s.copyFuncs = append(s.copyFuncs, func() error {
		_, err := io.Copy(s.ch, stdin)
		if err1 := s.ch.CloseWrite(); err == nil && err1 != io.EOF {
			err = err1
		}
		return err
	})
}

func (s *Session) stdout() {
	if s.stdoutpipe {
		return
	}
	if s.Stdout == nil {
		s.Stdout = io.Discard
	}
	s.copyFuncs = append(s.copyFuncs, func() error {
		_, err := io.Copy(s.Stdout, s.ch)
		return err
	})
}

func (s *Session) stderr() {
	if s.stderrpipe {
		return
	}
	if s.Stderr == nil {
		s.Stderr = io.Discard
	}
	s.copyFuncs = append(s.copyFuncs, func() error {
		_, err := io.Copy(s.Stderr, s.ch.Stderr())
		return err
	})
}

// sessionStdin reroutes Close to CloseWrite.
type sessionStdin struct {
	io.Writer
	ch Channel
}

func (s *sessionStdin) Close() error {
	return s.ch.CloseWrite()
}

// StdinPipe returns a pipe that will be connected to the
// remote command's standard input when the command starts.
func (s *Session) StdinPipe() (io.WriteCloser, error) {
	if s.Stdin != nil {
		return nil, errors.New("ssh: Stdin already set")
	}
	if s.started {
		return nil, errors.New("ssh: StdinPipe after process started")
	}
	s.stdinpipe = true
	return &sessionStdin{s.ch, s.ch}, nil
}

// StdoutPipe returns a pipe that will be connected to the
// remote command's standard output when the command starts.
// There is a fixed amount of buffering that is shared between
// stdout and stderr streams. If the StdoutPipe reader is
// not serviced fast enough it may eventually cause the
// remote command to block.
func (s *Session) StdoutPipe() (io.Reader, error) {
	if s.Stdout != nil {
		return nil, errors.New("ssh: Stdout already set")
	}
	if s.started {
		return nil, errors.New("ssh: StdoutPipe after process started")
	}
	s.stdoutpipe = true
	return s.ch, nil
}

// StderrPipe returns a pipe that will be connected to the
// remote command's standard error when the command starts.
// There is a fixed amount of buffering that is shared between
// stdout and stderr streams. If the StderrPipe reader is
// not serviced fast enough it may eventually cause the
// remote command to block.
func (s *Session) StderrPipe() (io.Reader, error) {
	if s.Stderr != nil {
		return nil, errors.New("ssh: Stderr already set")
	}
	if s.started {
		return nil, errors.New("ssh: StderrPipe after process started")
	}
	s.stderrpipe = true
	return s.ch, nil
}

// newSession returns a new interactive session on the remote host.
func newSession(conn *transport) (*Session, error) {
	s := &Session{
		conn: conn,
		ch:   conn.newChannel("", 0, []byte{}),
	}
	s.exitStatus = make(chan error, 1)
	go func() {
		s.exitStatus <- s.wait()
	}()

	return s, nil
}

// An ExitError reports unsuccessful completion of a remote command.
type ExitError struct {
	Waitmsg
}

func (e *ExitError) Error() string {
	return e.Waitmsg.String()
}

// Waitmsg stores the information about an exited remote command
// as reported by Wait.
type Waitmsg struct {
	status int
	signal string
	msg    string
	lang   string
}

// ExitStatus returns the exit status of the remote command.
func (w Waitmsg) ExitStatus() int {
	return w.status
}

// Signal returns the exit signal of the remote command if
// it was terminated violently.
func (w Waitmsg) Signal() string {
	return w.signal
}

// Msg returns the exit message given by the remote command
func (w Waitmsg) Msg() string {
	return w.msg
}

// Lang returns the language tag. See RFC 3066
func (w Waitmsg) Lang() string {
	return w.lang
}

func (w Waitmsg) String() string {
	str := fmt.Sprintf("Process exited with status %v", w.status)
	if w.signal != "" {
		str += fmt.Sprintf(" from signal %v", w.signal)
	}
	if w.msg != "" {
		str += fmt.Sprintf(". Reason was: %v", w.msg)
	}
	return str
}

// SendRequest sends an out-of-band channel request on the SSH channel
// underlying the session.
func (s *Session) SendRequest(name string, wantReply bool, payload []byte) (bool, error) {
	return s.ch.SendRequest(name, wantReply, payload)
}
