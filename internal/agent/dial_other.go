//go:build !linux

package agent

import "syscall"

func bindDefaultInterface(_, _ string, _ syscall.RawConn) error {
	return nil
}
