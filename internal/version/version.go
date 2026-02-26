package version

// Version is the current version of the tool.
var Version = "0.0.pkg"

// Hash is the Git hash of the current commit for pinning source code.
var Hash = ""

// BuiltDate is the date of compilation.
var BuiltDate = ""

// GetHash returns the Git hash relating to the build, or an empty string.
func GetHash() string {
	hash := Hash
	if len(hash) > 8 {
		hash = hash[:8]
	}
	return hash
}
