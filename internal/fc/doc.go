// Package fc wraps the firecracker-go-sdk Machine API with firefork-specific
// helpers: configuration defaults, jailer chroot management, snapshot create
// and restore, and (optionally) a userfaultfd handler for fast restore.
package fc
