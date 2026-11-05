//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package chatgptauth

import "os"

// crossProcessLocking 为 false：当前平台没有接入跨进程文件锁，只保留进程内互斥，
// 后台刷新因此拒绝启动（DESIGN 7.1）。
const crossProcessLocking = false

func tryLockFile(*os.File) (bool, error) { return true, nil }

func unlockFile(*os.File) {}
