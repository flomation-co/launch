package config

import (
	"bytes"
	"os"
	"testing"

	. "github.com/onsi/gomega"
)

func Test_LoadConfig(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg, err := LoadConfig("../../test-config.json")
	Expect(err).To(BeNil())
	Expect(cfg).To(Not(BeNil()))
}

func Test_LoadConfigBadPath(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	cfg, err := LoadConfig("some-bad-path")
	Expect(err).To(Not(BeNil()))
	Expect(cfg).To(BeNil())
}

func Test_LoadConfigNotJSON(t *testing.T) {
	t.Parallel()
	RegisterTestingT(t)

	b := bytes.NewBufferString("").Bytes()

	err := os.WriteFile("not-json-config", b, os.ModePerm)
	Expect(err).To(BeNil())

	cfg, err := LoadConfig("not-json-config")
	Expect(err).To(Not(BeNil()))
	Expect(cfg).To(BeNil())

	err = os.Remove("not-json-config")
	Expect(err).To(BeNil())
}
