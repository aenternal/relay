package importer

import (
	"fmt"
	"strings"
)

// MissingImportError separates virtual import metadata from arbitrary compiler
// error text. Consumers may show Path while omitting source literals contained
// in other errors. Directories are local capabilities and stay out of UI DTOs.
type MissingImportError struct {
	Path        string
	Directories []string
}

func (e *MissingImportError) Error() string {
	if len(e.Directories) == 0 {
		return fmt.Sprintf("import %q not found; provide it in the selected sources", e.Path)
	}
	return fmt.Sprintf("import %q not found; checked include directories: %s", e.Path, strings.Join(e.Directories, ", "))
}
