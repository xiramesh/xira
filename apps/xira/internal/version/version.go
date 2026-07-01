package version

import "fmt"

const (
	Name = "xira"
)

var (
	Version = "0.2.0-dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func String() string {
	return fmt.Sprintf("%s %s commit=%s date=%s", Name, Version, Commit, Date)
}
