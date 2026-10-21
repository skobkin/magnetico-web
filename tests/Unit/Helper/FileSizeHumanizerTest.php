<?php

declare(strict_types=1);

namespace App\Tests\Unit\Helper;

use App\Helper\FileSizeHumanizer;
use PHPUnit\Framework\Attributes\DataProvider;
use PHPUnit\Framework\TestCase;

class FileSizeHumanizerTest extends TestCase
{
    #[DataProvider('sizesProvider')]
    public function testHumanizesSizesWithDecimalPrefixes(int $bytes, string $expected): void
    {
        $humanizer = new FileSizeHumanizer();

        self::assertSame($expected, $humanizer->humanize($bytes));
    }

    public static function sizesProvider(): iterable
    {
        yield 'bytes' => [0, '0.00 B'];
        yield 'less than kilobyte' => [999, '999.00 B'];
        yield 'kilobytes' => [1500, '1.50 kB'];
        yield 'megabytes' => [5 * 1000 * 1000, '5.00 MB'];
        yield 'gigabytes' => [3 * 1000 * 1000 * 1000, '3.00 GB'];
        yield 'terabytes' => [2 * 1000 * 1000 * 1000 * 1000, '2.00 TB'];
    }

    public function testUsesBinaryDividerWhenConfigured(): void
    {
        $humanizer = new FileSizeHumanizer(binaryPrefix: true);

        self::assertSame('1.46 kB', $humanizer->humanize(1500));
    }

    public function testRespectsDecimalsArgument(): void
    {
        $humanizer = new FileSizeHumanizer();

        self::assertSame('1.5 kB', $humanizer->humanize(1500, decimals: 1));
    }
}
