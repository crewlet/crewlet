package httpx

// MaxResponseBody bounds a decoded third-party app response.
//
// It caps the SUCCESS decode; the error body's cap is [RefusalBytes]. A
// compromised or malfunctioning endpoint must not choose this process's memory
// ceiling.
//
// 32 MiB, from the largest page any client asks for: 200 items at GitHub's and
// GitLab's maximum page size, an expanded issue at ~64 KiB, so ~13 MiB, with
// headroom for an endpoint that ignores a page-size request.
const MaxResponseBody = 32 << 20
