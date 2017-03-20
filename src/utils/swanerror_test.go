package utils

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewError(t *testing.T) {
	e := NewError(SeverityHigh, errors.New("foobar"))
	assert.NotNil(t, e)
}

func TestNewErrorFromString(t *testing.T) {
	e := NewErrorFromString(SeverityHigh, "foobar")
	assert.NotNil(t, e)
}
