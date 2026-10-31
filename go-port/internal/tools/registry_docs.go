package tools

// DocsHandlers is the Docs tools registry.
func DocsHandlers() map[string]Handler {
	return map[string]Handler{
		"readDocument":    ReadDocument,
		"listDocuments":   ListDocuments,
		"searchDocuments": SearchDocuments,
		"appendText":      AppendText,
		"insertText":      InsertText,
		"findAndReplace":  FindAndReplace,
		"appendMarkdown":  AppendMarkdown,
		"createDocument":  CreateDocument,
	}
}
