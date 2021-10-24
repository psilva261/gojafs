package gojafs

import (
	"os/user"
)

const PathPrefix = "/mnt/goja"

func Group(u *user.User) (string, error) {
	return u.Gid, nil
}
