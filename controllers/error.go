package controllers

import (
	"strings"
)

// IsAWSCNINotFoundError will assert errors than can be caused by aws-cni not ready yet
func IsAWSCNINotFoundError(err error) bool {
	if err != nil && strings.Contains(err.Error(), "aws-cni") {
		return true
	}
	return false
}
