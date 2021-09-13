package controllers

import (
	"strings"
)

func IsAWSCNINotFoundError(err error) bool {
	if err != nil && strings.Contains(err.Error(), "aws-cni") {
		return true
	}
	return false
}
