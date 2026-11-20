# Authentication and authorization (`auth`)

Each account has a login mode, set by an admin: **SSO** (OpenID Connect) or **local** (email and password). An account accepts only one of the two. The login page offers "Sign in with SSO" and an email/password form for local accounts.

**Every account is identified by a company email** whose domain is in the allowed list (a panel setting). The domain is checked when an account is created and on every login, in both modes. If a domain is removed, its accounts can no longer sign in. The only exception is the master account.

## SSO accounts (OpenID Connect)

- **Identity provider:** any standard OIDC provider. Tested with Keycloak; only standard OIDC is used (discovery, standard claims), so Okta, Google Workspace, and others should work. OIDC was chosen over SAML for its smaller, less error-prone surface; a SAML-only provider can be brokered through Keycloak without changing the app.
- **Protocol:** Authorization Code + PKCE with a confidential client, scopes `openid profile email`, endpoints from `<issuer>/.well-known/openid-configuration`. The redirect URI is `<BASE_URL>/auth/callback`.
- **Identity:** the `sub` claim, and the token's `iss` must match the configured issuer.
- **Provisioning:** accounts are created on first login (just-in-time).
- **Company emails only**, checked twice:
  - in the identity provider: the realm should contain only company users, with self-registration off and any brokered external login restricted to the company domain;
  - in the app, on every login: the token must carry `email_verified: true` and an email in an allowed domain.
- **Pre-registered accounts:** an SSO account created by an admin, or converted from local, is bound to the `sub` on the first SSO login whose verified email matches.
- **Without `OIDC_*` configured:** local accounts only.

MFA for SSO accounts is whatever the identity provider enforces. Local accounts have no second factor.

## Local accounts

- **Creation:** only admins create them, with a company email (the login), name, and role.
- **No password is ever chosen by a human:**
  - on creation or reset, the system generates a 24-character random password (~140 bits, no ambiguous characters);
  - it is shown exactly once to the admin, expires after 72h if unused, and must be changed on first login;
  - the change generates another password, shown only to the user, so the admin never knows the final one;
  - users can get a new password at any time by entering the current one.
- **Storage:** argon2id with OWASP-recommended parameters. Plaintext is never stored or logged.
- **Failed attempts:** after N consecutive failures (default 5), the account waits (default 15 min). Both are configurable; the counter resets on success. Errors are generic and every attempt is audited.
- **Concurrent password checks** are limited (default 4), since each argon2 check uses a lot of memory. Requests beyond the limit get "try again in a moment".

## Switching login mode

- **Local → SSO:** the password hash is deleted, sessions are revoked, and the account is bound by email on its next SSO login.
- **SSO → local:** a temporary password is generated and shown once to the admin, and sessions are revoked.

## Master account

On startup, after migrations and under an advisory lock, the process checks whether the master account exists. If not, it:

1. creates the `admin` user (local, admin role, flagged as master);
2. generates a strong password and stores only its hash;
3. writes the password to a `0600` file inside the container (`/tmp/thehackerdispatch/master-password`), read with the `master-password` subcommand. Stdout only says how to read it, never the password;
4. deletes the file on the master's first login.

If it exists, nothing happens: restarts don't recreate the account or the password. To recover a lost password, see [Lost master password](deployment.md#lost-master-password).

**Rules:**

- Always local: it's the break-glass account for when SSO fails.
- No email; signs in as `admin`.
- Can't be converted to SSO, demoted, banned, or deleted from the panel.
- Its password can only be reset by **another** admin. No admin resets their own password from the panel (generating a new one by entering the current one is still allowed).
- Every master login is audited. Uniqueness is enforced by a partial unique index on `is_master`.

## Sessions

- Each session is a database row with the hash of its 256-bit random id, user, IP, browser, start time, and last access.
- The cookie is `HttpOnly`, `Secure`, and `SameSite=Lax`. CSRF is handled by Go's `http.CrossOriginProtection`, so forms need no tokens.
- Lifetime is configurable (default 12h).
- Killing a session deletes its row. Admins can kill one, all of a user's, or all sessions; users can sign out of all of theirs. Changing the password or login mode, banning, or demoting an account kills its sessions.

## Profile and bans

- **Nickname:** picked on first access, unique, 3–24 characters `[a-z0-9_-]`, changeable later.
- **Ban:** sets `banned_at`, revokes sessions, and blocks new logins by SSO subject or email.
- **Deleting a user** bans them and deletes their content; without the ban, an SSO account would come back on its next login.

## Roles and permissions

`member` or `admin`, managed in the panel.

| Action | member | admin |
|---|:-:|:-:|
| Read the board and threads | ✓ | ✓ |
| Comment and vote | ✓ | ✓ |
| Edit or delete own comments | ✓ | ✓ |
| Generate a new password for themselves (entering the current one) | ✓ | ✓ |
| See and kill their own sessions | ✓ | ✓ |
| See and kill any user's sessions | | ✓ |
| Delete any post, thread, or comment | | ✓ |
| Create local accounts; switch an account between SSO and local | | ✓ |
| Reset another account's password | | ✓ (never their own; the master's only by another admin) |
| Promote or demote admins | | ✓ (except the master) |
| Ban or delete users | | ✓ (except the master) |
| See users' emails (admin user list only) | | ✓ |
| Manage sources, sections, channels, prompts, and settings | | ✓ |
