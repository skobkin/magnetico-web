<?php
declare(strict_types=1);

namespace App\Helper;

class FileSizeHumanizer
{
    // Can't really exceed 'EB' on 64-bit platform but let it go
    private const SIZE_SUFFIXES = ['B','kB','MB','GB','TB','PB','EB','ZB','YB'];

    private const DIVIDER_BINARY = 1024;
    private const DIVIDER_COMMON = 1000;

    public function __construct(private readonly bool $binaryPrefix = false) {}

    public function humanize(int $bytes, int $decimals = 2, bool $forceBinary = false): string
    {
        $bytesString = (string) $bytes;

        $factor = (int) floor((strlen($bytesString) - 1) / 3);

        $isBinary = $forceBinary ?: $this->binaryPrefix;
        $sizeDivider = $isBinary ? self::DIVIDER_BINARY : self::DIVIDER_COMMON;

        $maxSuffixIndex = count(self::SIZE_SUFFIXES) - 1;

        $suffixIndex = min($maxSuffixIndex, $factor);

        $suffix = self::SIZE_SUFFIXES[$suffixIndex];

        return sprintf("%.{$decimals}f %s", $bytes / ($sizeDivider ** $suffixIndex), $suffix);
    }
}
