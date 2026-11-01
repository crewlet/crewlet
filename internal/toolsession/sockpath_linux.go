package toolsession

// maxSocketPath is the size of sockaddr_un.sun_path on linux, the terminating
// NUL included: a socket's path must be shorter than this.
const maxSocketPath = 108
