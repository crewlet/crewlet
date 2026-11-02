package toolsession

// maxSocketPath is the size of sockaddr_un.sun_path on darwin, the terminating
// NUL included: a socket's path must be shorter than this. Four bytes fewer
// than linux, under a temp root that is already about fifty characters long.
const maxSocketPath = 104
