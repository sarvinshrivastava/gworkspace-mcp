# Parity map — the exposed 20 tools

The service exposes exactly 20 MCP tools. Reference points:

- TS reference service: commit e8d3d40ce2f064f8244214f8e78ffd17cab06a5d
- Upstream tool pack: @a-bonus/google-docs-mcp@1.11.3

No new tools, no dropped tools, no renamed tools. `appendTableRows` stays
excluded and the Gmail tools and `setFilePermission` stay blocked, exactly as
in the TS service.

| # | tool | upstream reference file | Go implementation |
|---|------|------------------------|-------------------|
| 1 | readDocument | tools/docs/readGoogleDoc.js | internal/tools/readdocument.go |
| 2 | listDocuments | tools/drive/listGoogleDocs.js | internal/tools/listdocuments.go |
| 3 | searchDocuments | tools/drive/searchGoogleDocs.js | internal/tools/searchdocuments.go |
| 4 | appendText | tools/docs/appendToGoogleDoc.js | internal/tools/appendtext.go |
| 5 | insertText | tools/docs/insertText.js | internal/tools/inserttext.go |
| 6 | findAndReplace | tools/docs/findAndReplace.js | internal/tools/findandreplace.go |
| 7 | appendMarkdown | tools/utils/appendMarkdownToGoogleDoc.js | internal/tools/appendmarkdown.go |
| 8 | createDocument | tools/drive/createDocument.js | internal/tools/createdocument.go |
| 9 | readSpreadsheet | tools/sheets/readSpreadsheet.js | internal/tools (sheets) |
| 10 | listSpreadsheets | tools/sheets/listGoogleSheets.js | internal/tools (sheets) |
| 11 | writeSpreadsheet | tools/sheets/writeSpreadsheet.js | internal/tools (sheets) |
| 12 | appendRows | tools/sheets/appendSpreadsheetRows.js | internal/tools (sheets) |
| 13 | createSpreadsheet | tools/sheets/createSpreadsheet.js | internal/tools (sheets) |
| 14 | listDriveFiles | tools/drive/listDriveFiles.js | internal/tools/listdrivefiles.go |
| 15 | createFolder | tools/drive/createFolder.js | internal/tools (drive) |
| 16 | listEvents | tools/calendar/listEvents.js | internal/tools (calendar) |
| 17 | createEvent | tools/calendar/createEvent.js | internal/tools (calendar) |
| 18 | updateEvent | tools/calendar/updateEvent.js | internal/tools (calendar) |
| 19 | quickAddEvent | tools/calendar/quickAddEvent.js | internal/tools (calendar) |
| 20 | searchDriveFiles | tools/drive/searchDriveFiles.js | internal/tools/searchdrivefiles.go (legacy), internal/enforce/search.go (folder-confined agents) |

## Compatibility rules

- tools/list, denial messages, pipeline ordering and the 17-reason deny table
  are part of the observable contract and are ported verbatim.
- Boundary argument validation mirrors the TS SDK layer: AJV draft-07 subset
  over the served inputSchema, `allErrors`, errorsText formatting, before any
  handler runs.
- Error text from Google surfaces mirrors gaxios 7.3.1
  extractAPIErrorFromResponse, including the error-object spread precedence
  (an own `message` key wins over `errors[].message` joins) and the
  google-auth-library `Could not refresh access token: ` prefix on 403/404
  refreshes.
- healthz `upstreamTools` reports the pinned exposed set (20) rather than the
  upstream pack's full registration count (119); the TS service reports 119
  because it proxies the whole pack. This is the one disclosed semantic delta.
- JSON emission mirrors JSON.stringify: no HTML escaping, U+2028/U+2029 raw,
  numbers canonicalized to JS Number::toString, `undefined`-valued keys
  dropped, key order preserved.
- Strings are sliced and capped in UTF-16 code units, as in JS.
