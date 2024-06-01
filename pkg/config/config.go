package config

// DefaultConfig is a global variable that is initialized at startup with the flags options.
// It can not be modified after that.
var DefaultConfig = &Config{}

type Config struct {
	EnableLogDump bool
	LogDir        string
}
