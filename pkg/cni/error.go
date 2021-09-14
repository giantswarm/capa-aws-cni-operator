package cni

import (
	"strings"
)

// IsApiNotReadyYet will assert possible errors that can be caused wc k8s api not ready yet
func IsApiNotReadyYet(err error) bool {
	if err != nil && (strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "no such host")) {
		return true
	}
	return false
}

//IsENIConfigNotRegistered will assert and error when aws-cni did not registered ENIConfigs CRD
func IsENIConfigNotRegistered(err error) bool {
	if err != nil && (strings.Contains(err.Error(), "no matches for kind")) {
		return true
	}
	return false
}
