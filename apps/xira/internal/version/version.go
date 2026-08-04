package version

import "fmt"

const (
	Name = "xira"
)

var (
	Version = "0.8.2"
	Commit  = "unknown"
	Date    = "unknown"
)

func String() string {
	return fmt.Sprintf("%s %s commit=%s date=%s", Name, Version, Commit, Date)
}
