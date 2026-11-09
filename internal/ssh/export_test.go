package ssh

// NewRunnerDialing is a runner whose every dial reaches addr, for the tests
// outside this package that drive an agent's call to a real sshd.
func NewRunnerDialing(certs Recorder, commands CommandRecorder, addr string) *Runner {
	r := NewRunner(certs, commands)
	r.dial = dialTo(addr)
	return r
}
