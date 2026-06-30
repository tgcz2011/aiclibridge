// Package config loads aiclibridge's daemon configuration from a YAML
// file and applies environment-variable overrides (prefix AICLIBRIDGE_).
//
// Resolution order (see ResolveConfigPath): an explicit --config path,
// then $AICLIBRIDGE_CONFIG, then ./aiclibridge.yaml, then
// ~/.aiclibridge/config.yaml, then "" (use Defaults).
//
// Env overrides take precedence over the YAML file in all cases, so a
// single deployed binary can be re-tuned without editing the file.
package config
