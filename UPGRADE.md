# Upgrade notes

User-facing and breaking changes that require action when deploying a new version.
Entries are grouped by release date. Read the section covering your current version
before upgrading.

## 2026-10-21 0.7.0

### PHP 8.5 required

The application now requires PHP >= 8.5 (previously 8.2). The Docker image is based
on `dunglas/frankenphp:1-php8.5-alpine`. If you run the app outside Docker, upgrade
your PHP runtime first.

### reCAPTCHA bundle replaced (env vars renamed)

`excelwebzone/recaptcha-bundle` was replaced with `karser/karser-recaptcha3-bundle`
(reCAPTCHA v3). Rename your environment variables:

| Old variable             | New variable       |
|--------------------------|--------------------|
| `EWZ_RECAPTCHA_SITE_KEY` | `RECAPTCHA3_KEY`   |
| `EWZ_RECAPTCHA_SECRET`   | `RECAPTCHA3_SECRET`|

reCAPTCHA v3 keys are issued per site; if your existing keys were registered for
v3, their values can be reused directly. Otherwise register the site for
[reCAPTCHA v3](https://www.google.com/recaptcha/admin/create) and use the new keys.
reCAPTCHA is now also enforced on the registration form (previously only on the
password reset request form).

### FrankenPHP runtime package removed

`runtime/frankenphp-symfony` is no longer used — `symfony/runtime` >= 7.4 detects
FrankenPHP worker mode natively. If you set `APP_RUNTIME=Runtime\FrankenPhpSymfony\Runtime`
in your environment (e.g. in a custom `docker-compose.yml`), remove that variable.
The bundled Dockerfile no longer sets it.

### Symfony 8 upgrade: re-login required

The upgrade to Symfony 8 changes the remember-me cookie format. All logged-in
users (including "remember me" sessions) will be logged out once after the deploy
and must log in again. No stored data is affected.

### Before you deploy

1. **Back up both PostgreSQL databases** — the application database
   (`APP_DATABASE_URL`) and the magneticod database (`MAGNETICOD_DATABASE_URL`).
2. Deploy the new version. This release contains **no database schema changes**
   and no new migrations.
3. After the deploy, verify both entity managers:
   `bin/console doctrine:schema:validate --em=default` and
   `bin/console doctrine:schema:validate --em=magneticod`.
