<?php
// Generates legacy-compatible password hash vectors with PHP's own
// implementations. No network, no database, no credentials: every password
// below is synthetic. Output: JSON on stdout (see README.md for the command).
declare(strict_types=1);

function bytes_of(string $kind): string {
    switch ($kind) {
        case 'ascii':     return 'correct horse battery staple';
        case 'utf8':      return "p\xc3\xa4ssw\xc3\xb6rd-\xe6\x97\xa5\xe6\x9c\xac\xe8\xaa\x9e-\xf0\x9f\x94\x91";
        case 'len1':      return 'a';
        case 'len8':      return 'abcdefgh';
        case 'len72':     return substr(str_repeat('0123456789abcdefghijklmnopqrstuvwxyz', 3), 0, 72);
        case 'len73':     return substr(str_repeat('0123456789abcdefghijklmnopqrstuvwxyz', 3), 0, 73);
        case 'len100':    return substr(str_repeat('0123456789abcdefghijklmnopqrstuvwxyz', 3), 0, 100);
        case 'len4096':   return substr(str_repeat('0123456789abcdefghijklmnopqrstuvwxyz', 120), 0, 4096);
    }
    throw new RuntimeException("unknown password kind $kind");
}

$ALL = ['ascii', 'utf8', 'len1', 'len8', 'len72', 'len73', 'len100', 'len4096'];

// Each format: label, family, params description, hasher, password kinds.
$formats = [
    ['argon2i_m16384_t2_p4', 'argon2i', ['m' => 16384, 't' => 2, 'p' => 4],
        fn(string $p) => password_hash($p, PASSWORD_ARGON2I, ['memory_cost' => 16384, 'time_cost' => 2, 'threads' => 4]), $ALL],
    ['argon2id_sodium_m65536_t4_p1', 'argon2id', ['m' => 65536, 't' => 4, 'p' => 1],
        fn(string $p) => sodium_crypto_pwhash_str($p, 4, 67108864), $ALL],
    ['argon2id_sodium_interactive_m65536_t2_p1', 'argon2id', ['m' => 65536, 't' => 2, 'p' => 1],
        fn(string $p) => sodium_crypto_pwhash_str($p, SODIUM_CRYPTO_PWHASH_OPSLIMIT_INTERACTIVE, SODIUM_CRYPTO_PWHASH_MEMLIMIT_INTERACTIVE), ['ascii', 'utf8', 'len72']],
    ['argon2id_php_default', 'argon2id', ['m' => PASSWORD_ARGON2_DEFAULT_MEMORY_COST, 't' => PASSWORD_ARGON2_DEFAULT_TIME_COST, 'p' => PASSWORD_ARGON2_DEFAULT_THREADS],
        fn(string $p) => password_hash($p, PASSWORD_ARGON2ID), ['ascii', 'utf8', 'len72']],
    ['argon2id_php_m65536_t3_p2', 'argon2id', ['m' => 65536, 't' => 3, 'p' => 2],
        fn(string $p) => password_hash($p, PASSWORD_ARGON2ID, ['memory_cost' => 65536, 'time_cost' => 3, 'threads' => 2]), ['ascii', 'utf8']],
    ['bcrypt_2y_cost10', 'bcrypt', ['cost' => 10],
        fn(string $p) => password_hash($p, PASSWORD_BCRYPT, ['cost' => 10]), $ALL],
    ['bcrypt_2y_cost13', 'bcrypt', ['cost' => 13],
        fn(string $p) => password_hash($p, PASSWORD_BCRYPT, ['cost' => 13]), ['ascii', 'len73']],
];

$cases = [];
foreach ($formats as [$label, $family, $params, $hasher, $kinds]) {
    foreach ($kinds as $kind) {
        $pw = bytes_of($kind);
        $hash = $hasher($pw);
        // wrong: first byte changed.
        $wrong = chr((ord($pw[0]) + 1) % 256) . substr($pw, 1);
        // variant: the original plus a tail. Verifies only when the algorithm
        // ignores input beyond 72 bytes (bcrypt) and the password is >= 72 bytes.
        $variant = $pw . '-tail';
        $case = [
            'id' => "$label/$kind",
            'family' => $family,
            'params' => $params,
            'password_len' => strlen($pw),
            'password_b64' => base64_encode($pw),
            'hash' => $hash,
            'verifies' => password_verify($pw, $hash),
            'wrong_password_b64' => base64_encode($wrong),
            'wrong_verifies' => password_verify($wrong, $hash),
            'tail_variant_b64' => base64_encode($variant),
            'tail_variant_verifies' => password_verify($variant, $hash),
        ];
        if (strlen($pw) > 72) {
            $prefix = substr($pw, 0, 72);
            $case['prefix72_b64'] = base64_encode($prefix);
            $case['prefix72_verifies'] = password_verify($prefix, $hash);
        }
        $cases[] = $case;
    }
}

echo json_encode([
    'meta' => [
        'generated_utc' => gmdate('c'),
        'php_version' => PHP_VERSION,
        'password_argon2_provider' => PASSWORD_ARGON2_PROVIDER,
        'sodium_library_version' => SODIUM_LIBRARY_VERSION,
        'sodium_extension_version' => phpversion('sodium'),
        'openssl_version' => OPENSSL_VERSION_TEXT,
        'note' => 'synthetic passwords only; salts are random, so regenerating yields different hashes',
    ],
    'cases' => $cases,
], JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES), "\n";
