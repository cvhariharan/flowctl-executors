package executorutil

import (
	"fmt"
	"regexp"
	"strings"
)

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// SubstituteEnvVars replaces ${VAR} occurrences in s with values from inputs.
// Unknown keys are left as-is.
func SubstituteEnvVars(s string, inputs map[string]any) string {
	return envVarRe.ReplaceAllStringFunc(s, func(m string) string {
		key := strings.TrimSpace(envVarRe.FindStringSubmatch(m)[1])
		if v, ok := inputs[key]; ok {
			return fmt.Sprintf("%v", v)
		}
		return m
	})
}
