package envflag

import (
	"flag"
	"log"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
)

var m sync.Mutex
var knownEnvs = map[string]string{}

func register(flagName string, envName string) {
	m.Lock()
	defer m.Unlock()
	if knownFlag, isAlreadySet := knownEnvs[envName]; isAlreadySet {
		log.Fatalf(
			"cant register env '%s' for flag '%s', because it is already in use for flag %s",
			envName, flagName, knownFlag,
		)
	}
	knownEnvs[envName] = flagName
}

func normalizeEnv(name string) string {
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ToUpper(name)
	return name
}

func getAndRegisterStringEnv(flagName string, defaultValue string) string {
	envName := normalizeEnv(flagName)
	register(flagName, envName)
	envValue := os.Getenv(envName)
	if envValue == "" {
		return defaultValue
	}
	slog.Info(
		"envflag: use env variable",
		slog.String("env_name", envName),
		slog.String("flag_name", flagName),
	)
	return envValue
}

func getAndRegisterIntEnv(flagName string, defaultValue int) int {
	envName := normalizeEnv(flagName)
	register(flagName, envName)
	envValue := os.Getenv(envName)
	if envValue == "" {
		return defaultValue
	}
	slog.Info(
		"envflag: use env variable",
		slog.String("env_name", envName),
		slog.String("flag_name", flagName),
	)
	result, err := strconv.Atoi(os.Getenv(envName))
	if err != nil {
		log.Fatalf("envflag: fail to parse as int env '%s' with value '%s': %s", envName, envValue, err.Error())
	}
	return result
}

func StringVar(p *string, name string, value string, usage string) {
	flag.StringVar(p, name, getAndRegisterStringEnv(name, value), usage)
}

func IntVar(p *int, name string, value int, usage string) {
	flag.IntVar(p, name, getAndRegisterIntEnv(name, value), usage)
}

func Parse() {
	flag.Parse()
}

// KnownEnvs return flag name to env name
func KnownEnvs() map[string]string {
	result := map[string]string{}
	m.Lock()
	for envName, flagName := range knownEnvs {
		result[flagName] = envName
	}
	m.Unlock()
	return result
}
