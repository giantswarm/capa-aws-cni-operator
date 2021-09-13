package cni

import (
	"strings"
)

func IsApiNotReadyYet(err error) bool {
	if err != nil && (strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "no such host")) {
		return true
	}
	return false
}

func IsENIConfigNotRegistered(err error) bool {
	if err != nil && (strings.Contains(err.Error(), "no matches for kind")) {
		return true
	}
	return false
}
