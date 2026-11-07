package ryzensmucli

// pinnedProvenance is the fixture manifest for the exact tool build
// these parsers were written against (audit B12). It pins SOURCE
// identity (repo, tag, commit, the Program.cs blob the output shapes
// were read from), the release ARCHIVE (asset name, size, SHA-256),
// and - since the read-only smoke on real hardware (2026-10-18) -
// the MEASURED installed-file digests (the .NET apphost exe and the
// managed payload DLL). The archive hash pins the whole zip; the
// per-file digests let Probe confirm the installed launcher is that
// exact build rather than an arbitrary exe that merely runs.
//
// Provenance of these values: tag and commit read from the release
// channel's GitHub API on 2026-10-18; the archive SHA-256 and size
// measured from the downloaded ryzen-smu-cli-0.1.3.zip on
// 2026-10-18; the exe and DLL SHA-256 measured from the unpacked
// 0.1.3 install on 2026-10-18; Program.cs shapes read from the blob
// at this exact commit, then CONFIRMED against the real build's
// --version and --help output on 2026-10-18: the built-in
// System.CommandLine --version prints the bare informational version
// "<semver>+<git sha>" (no name prefix), and the embedded commit
// equals the pinned commit - so the version string itself carries
// SourceLink source identity.
var pinnedProvenance = struct {
	Repo        string // source repository the tool's author maintains
	ReleaseRepo string // release channel carrying the pinned build
	Tag         string
	Commit      string // tag 0.1.3 -> this commit (lightweight tag object)
	ProgramBlob string // ryzen-smu-cli/Program.cs blob sha at that commit
	Asset       string
	AssetSize   int64
	AssetSHA256 string
	ExeSHA256   string // apphost launcher, measured in the unpacked 0.1.3 install
	DllSHA256   string // managed payload (the real code), measured in the install
	AssemblyVer string // <Version> in ryzen-smu-cli.csproj at that commit
	SyntaxMin   int    // argument regex -?\d{1,2}: what the tool PARSES
	SyntaxMax   int
}{
	Repo:        "github.com/sp00n/ryzen-smu-cli",
	ReleaseRepo: "github.com/rawhide-kobayashi/ryzen-smu-cli",
	Tag:         "0.1.3",
	Commit:      "39413e27658223b27470196ed0f2b35c1d966899",
	ProgramBlob: "c5d5e866b318e2e9acbd92908aab73589caa4a2b",
	Asset:       "ryzen-smu-cli-0.1.3.zip",
	AssetSize:   632207,
	AssetSHA256: "0131955f780566d43f464350c65de7f929dd9cb57a583c401fe7545712e98ffb",
	ExeSHA256:   "1d16aaba23c222f1f76f1ecae35a43bcd50291597d8c3877536f327ef3b62fbf",
	DllSHA256:   "89cc728ef2f2a007d6c9272e2c1566c5b424e2dbc0cbac0f408a8066188e8aab",
	AssemblyVer: "0.1.3",
	SyntaxMin:   -99,
	SyntaxMax:   99,
}
