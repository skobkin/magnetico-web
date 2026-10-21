# Upgrade notes

User-facing and breaking changes that require action when deploying a new version.
Entries are grouped by release date. Read the section covering your current version
before upgrading.

## 2026-10-21 0.7.0

### PHP 8.5 required

The application now requires PHP >= 8.5 (previously 8.2). The Docker image is based
on `dunglas/frankenphp:1-php8.5-alpine`. If you run the app outside Docker, upgrade
your PHP runtime first.

### PHP security floor: 8.5.12 (critical RCE fixes, CVE-2026-109618 / CVE-2026-109619)

PHP 8.5.12, 8.4.27, 8.3.36 and 8.2.35 (released 2026-10-21) fix two critical
unauthenticated remote code execution vulnerabilities affecting all web-serving
SAPIs, including FrankenPHP (see the
[PHP announcement](https://www.php.net/archive/2026.php#2026-10-21-5)). The Docker
image in this release is built on PHP 8.5.12 and **refuses to build** on an older
base image. If you run the app outside Docker, make sure your runtime is 8.5.12+
(or 8.4.27+/8.3.36+/8.2.35+ on older branches) and update immediately — per the
advisory there are no complete mitigations, updating is the only fix.

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
