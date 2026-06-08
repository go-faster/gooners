//go:build !linux

package main

import "os/exec"

func setProcAttr(_ *exec.Cmd) {}
