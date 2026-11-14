package core_test

import (
	"syscall"

	"github.com/labsbit/rov/internal/config"
)

var syscallSignal0 = syscall.Signal(0)

func envOf(kv ...string) []config.EnvVar {
	var out []config.EnvVar
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, config.EnvVar{Key: kv[i], Value: kv[i+1]})
	}
	return out
}
