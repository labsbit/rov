package core

import "syscall"

// SetKill replaces the signal sender of stopTree (nil: syscall.Kill).
func SetKill(e *Engine, kill func(pid int, sig syscall.Signal) error) { e.kill = kill }

var (
	SelfInTree = selfInTree
	StillSame  = stillSame
)
