-- Task 07H — read-only, aggregate-only audit of the LEGACY PHP database.
--
-- The owner runs this separately, with psql, against the legacy database
-- (schema "users"), for example:
--
--     psql "$LEGACY_DSN" -X -f docs/wip/07H-audit.sql
--
-- It never writes (READ ONLY transaction, rolled back). Its output contains
-- only counts and parameters: no password hash (not even a salt), no
-- username, no email, no token, no invitation code. The task assignment, the
-- plan (§3 D1) and the migrator's own dry-run are authoritative; the category
-- column here is an advisory SQL re-implementation of
-- internal/migration/users CategorizeHash and is tested against it.
--
-- category:
--   verified     exact parameters with independently generated PHP vectors
--                (internal/web/password/testdata/php/fixtures.json):
--                argon2i m=16384,t=2,p=4 | argon2id m=65536 with t=4,p=1,
--                t=2,p=1 or t=3,p=2 | $2y$ cost 10 or 13
--   unvetted     the verifier would parse it (v=19, m<=65536 KiB, t<=10,
--                p<=16, salt 8-64 B, key 16-64 B; $2y$ cost 4-16) but there
--                are NO exact PHP vectors: login would work, production
--                compatibility is NOT approved until vectors exist; the final
--                migration refuses these accounts
--   unsupported  needs-reset: cannot log in (an administrator-issued reset
--                link is the only recovery), and completing that reset
--                permanently revokes the user's legacy RSS URLs
--   empty        not a hash at all: the migration refuses it

BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;

-- 1. Password-hash formats and parameters (aggregate).
WITH parsed AS (
  SELECT password AS h,
         CASE
           WHEN password ~ '^\$argon2(id|i|d)\$v=[0-9]+\$m=[0-9]+,t=[0-9]+,p=[0-9]+\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$' THEN 'argon2'
           WHEN password ~ '^\$2[abxy]\$[0-9]{2}\$[./A-Za-z0-9]{53}$' THEN 'bcrypt'
           WHEN password = '' THEN 'empty'
           ELSE 'unrecognized'
         END AS family
  FROM users.users
), f AS (
  SELECT h, family,
         CASE family WHEN 'argon2' THEN split_part(h, '$', 2) WHEN 'bcrypt' THEN substr(h, 2, 2) END AS variant,
         CASE family WHEN 'argon2' THEN split_part(h, '$', 3) END AS version,
         CASE family WHEN 'argon2' THEN split_part(h, '$', 4) WHEN 'bcrypt' THEN 'cost=' || substr(h, 5, 2) END AS params,
         CASE family WHEN 'argon2' THEN length(split_part(h, '$', 5)) END AS salt_b64_len,
         CASE family WHEN 'argon2' THEN length(split_part(h, '$', 6)) END AS key_b64_len,
         CASE family WHEN 'argon2' THEN substring(split_part(h, '$', 4) FROM 'm=([0-9]+)')::numeric END AS m,
         CASE family WHEN 'argon2' THEN substring(split_part(h, '$', 4) FROM 't=([0-9]+)')::numeric END AS t,
         CASE family WHEN 'argon2' THEN substring(split_part(h, '$', 4) FROM 'p=([0-9]+)')::numeric END AS p
  FROM parsed
), c AS (
  SELECT *,
         CASE
           WHEN family = 'empty' THEN 'empty'
           WHEN family IN ('argon2', 'bcrypt')
                AND (variant || '/' || COALESCE(version || '/', '') || params) IN (
                  'argon2i/v=19/m=16384,t=2,p=4',
                  'argon2id/v=19/m=65536,t=4,p=1', 'argon2id/v=19/m=65536,t=2,p=1', 'argon2id/v=19/m=65536,t=3,p=2',
                  '2y/cost=10', '2y/cost=13') THEN 'verified'
           WHEN family = 'argon2' AND variant IN ('argon2i', 'argon2id') AND version = 'v=19'
                AND p BETWEEN 1 AND 16 AND t BETWEEN 1 AND 10 AND m BETWEEN 8 * p AND 65536
                AND salt_b64_len BETWEEN 11 AND 86 AND salt_b64_len % 4 <> 1
                AND key_b64_len BETWEEN 22 AND 86 AND key_b64_len % 4 <> 1 THEN 'unvetted'
           WHEN family = 'bcrypt' AND variant = '2y' AND substr(h, 5, 2)::int BETWEEN 4 AND 16 THEN 'unvetted'
           ELSE 'unsupported'
         END AS category
  FROM f
)
SELECT category, family, variant, version, params, salt_b64_len, key_b64_len,
       CASE family WHEN 'unrecognized' THEN (h LIKE '$%') END AS starts_with_dollar,
       CASE family WHEN 'unrecognized' THEN width_bucket(length(h), 0, 256, 8) END AS length_bucket,
       count(*) AS accounts
FROM c
GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9
ORDER BY accounts DESC, 1, 2, 3, 4, 5;

-- 2. Data shape (counts only).
SELECT
  (SELECT count(*) FROM users.users) AS users,
  (SELECT count(*) FROM users.invites) AS invites,
  (SELECT count(*) FROM users.invites WHERE used_by_id IS NOT NULL) AS used_invites,
  (SELECT count(*) FROM users.api_tokens) AS api_tokens,
  (SELECT COALESCE(max(n), 0) FROM (SELECT count(*) AS n FROM users.api_tokens GROUP BY user_id) x) AS max_tokens_per_user,
  (SELECT count(*) FROM (SELECT user_id FROM users.api_tokens GROUP BY user_id HAVING count(*) > 50) x) AS users_over_50_tokens,
  (SELECT count(*) FROM (SELECT lower(email) FROM users.users GROUP BY 1 HAVING count(*) > 1) x) AS case_duplicate_email_groups,
  (SELECT count(*) FROM users.users WHERE email !~ '^[^@[:space:]]+@[^@[:space:]]+$') AS emails_not_simple,
  (SELECT count(*) FROM users.users WHERE char_length(email) < 3 OR char_length(username) < 1 OR password = '') AS unstorable_user_values,
  (SELECT count(*) FROM users.invites WHERE code !~ '^[A-Za-z0-9_-]{22,64}$') AS invalid_invite_codes,
  (SELECT count(*) FROM users.api_tokens WHERE key = '') AS empty_token_keys,
  (SELECT count(*) FROM users.invites i WHERE NOT EXISTS (SELECT 1 FROM users.users u WHERE u.id = i.user_id)) AS orphan_invite_owners,
  (SELECT count(*) FROM users.invites i WHERE i.used_by_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM users.users u WHERE u.id = i.used_by_id)) AS orphan_used_by,
  (SELECT count(*) FROM users.api_tokens t WHERE NOT EXISTS (SELECT 1 FROM users.users u WHERE u.id = t.user_id)) AS orphan_token_owners,
  (SELECT count(*) FROM users.users WHERE roles::text !~ '^\s*\[\s*"ROLE_USER"\s*\]\s*$') AS roles_not_plain_user;

ROLLBACK;
