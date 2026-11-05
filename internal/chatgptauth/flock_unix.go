//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package chatgptauth

import (
	"errors"
	"os"
	"syscall"
)

// crossProcessLocking 表示当前平台用 flock 提供跨进程排他锁。
const crossProcessLocking = true

// tryLockFile 以非阻塞方式对已打开的锁文件加排他锁；被其他持有者占用时返回 false。
// flock 绑定在打开的文件描述上，同一进程内两次打开同一文件也会互斥，进程退出时由内核释放。
func tryLockFile(file *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN):
			return false, nil
		default:
			return false, err
		}
	}
}

func unlockFile(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
