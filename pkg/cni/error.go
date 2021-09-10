package cni

import (
	"strings"

	"github.com/aws/aws-sdk-go/aws/awserr"
)

func IsNotFound(err error) bool {
	if aerr, ok := err.(awserr.Error); ok {
		if strings.Contains(aerr.Message(), "NotFound") {
			return true
		}
	}
	return false
}

func IsAlreadyExists(err error) bool {
	if aerr, ok := err.(awserr.Error); ok {
		if strings.Contains(aerr.Message(), "AlreadyExist") {
			return true
		}
	}
	return false
}

func IsApiNotReadyYet(err error) bool {
	if err != nil && (strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "no such host")) {
		return true
	}
	return false
}
