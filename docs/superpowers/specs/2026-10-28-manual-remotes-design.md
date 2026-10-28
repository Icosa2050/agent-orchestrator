# Independent manual remote hosts

The user approved restoring manual hosts in the current fork, retaining security checks and optional cloud discovery, with tests and a draft PR.

Manual pairing must work without an AO account. New manual credentials stay local, including when paired while signed in; adding or editing them must not publish an account token. Account sign-in remains an optional way to discover account-linked machines. Existing account-owned records remain scoped to their owner; they must not silently become local credentials on logout. Explicit pairing with a supplied password can replace an owned record.

Retain host identity/version checks before presenting credentials, self-connection refusal, local-identity restart handling, file permission checks, account-transition checks, and authenticated remote listeners. Cloud import/pruning must never modify a local manual record, including a password that happens to be 64 hex characters. A cloud failure must not block manual CRUD or connection.

## Approach

Restore the local path in the current Electron IPC, settings form, and connection hook. Keep account discovery active only when signed in; sign-in is optional and explained in the settings page. Preserve cloud account operations separately. Mark account-owned views so removing a manual host never calls the cloud API.

A full downgrade loses subsequent fixes. Reverting all of #6204 also removes unrelated cloud/mobile work. This focused restoration preserves both paths without schema migrations or changing the running app.
