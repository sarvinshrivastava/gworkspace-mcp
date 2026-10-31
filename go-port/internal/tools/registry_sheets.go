package tools

// SheetsHandlers is the Sheets tools registry; the gateway core merges it
// with the other tools' maps.
func SheetsHandlers() map[string]Handler {
	return map[string]Handler{
		"readSpreadsheet":   ReadSpreadsheet,
		"listSpreadsheets":  ListSpreadsheets,
		"writeSpreadsheet":  WriteSpreadsheet,
		"appendRows":        AppendRows,
		"createSpreadsheet": CreateSpreadsheet,
	}
}
