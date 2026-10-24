# Privacy policy

Last updated: 2026-10-24

`gworkspace-mcp` is a personal, single-owner tool run by Sarvin Shrivastava (sarvin5124@gmail.com). It lets the owner's own AI agents read and edit the owner's own Google Docs, Sheets, Drive files and Calendar through one gateway. It is not offered to the public and has one user: its owner.

## What it accesses
Only the owner's Google account, with the owner's consent, through Google's OAuth. It uses the Google Drive, Docs, Sheets, Calendar and Gmail APIs, and only for the actions the owner's agents request.

## What it stores
- A Google OAuth refresh token, kept in the owner's private secrets vault and written to memory-backed storage at start-up. It is never committed or logged.
- An audit log on the owner's own server: which agent called which tool, when, the outcome, and Google file IDs. It does not store document, spreadsheet, e-mail or calendar content.

## Who it is shared with
No one. Data is not sold, not used for advertising, not used to train models by this tool, and not shared with third parties.
- Content fetched from Google goes only to the owner's own agents that asked for it.
- Operational alerts (for example "Google access rejected") are sent to a notification topic the owner controls. They contain a status and a time, never Google content or personal data.

## Google API Services User Data Policy
The use and transfer of information received from Google APIs to any other app adheres to the [Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy), including its Limited Use requirements.

## Revoking access and deleting data
The owner can revoke access at any time at https://myaccount.google.com/permissions and by deleting the token from the vault. Deleting the server's state volume removes the audit log.

## Contact
sarvin5124@gmail.com
