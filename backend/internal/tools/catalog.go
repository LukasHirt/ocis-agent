package tools

// NewDefault returns the registry of all tools of this version.
func NewDefault() (*Registry, error) {
	return NewRegistry(
		NewListSpaces(), NewList(), NewStat(), NewReadText(),
		NewCreateFolder(), NewWriteNew(), NewOverwrite(), NewMove(), NewCopy(), NewDelete(),
		NewMemoryList(), NewMemoryRead(), NewMemoryWrite(),
	)
}
