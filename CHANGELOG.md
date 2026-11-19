# Changelog

Record significant behavioural, API, or architectural changes here. Skip routine edits, refactors that do not change behaviour, and documentation that does not alter a published contract.

## Unreleased

- Initial vertical slice: device registration, hashed evidence upload, idempotent events, and an HTML/JSON timeline. The API contract is in the README.
- Windows activity agent (`cmd/agent`): polls the foreground window, registers once and reuses the saved bearer token, and submits `application.foreground` events backed by hashed JSON evidence.
