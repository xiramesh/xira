package version

import "fmt"

const (
	Name = "xira"
)

var (
	Version = "0.5.0"
	Commit  = "unknown"
	Date    = "unknown"
)

func String() string {
	return fmt.Sprintf("%s %s commit=%s date=%s", Name, Version, Commit, Date)
}
