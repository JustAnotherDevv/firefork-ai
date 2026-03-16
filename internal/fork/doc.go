// Package fork implements the CoW fork primitive: given a single source
// memfile, it spawns N Firecracker processes that each MAP_PRIVATE the same
// underlying file so writes diverge per-fork while clean pages stay shared.
package fork
