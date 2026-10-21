<?php

declare(strict_types=1);

namespace App\Tests\Unit\Magnet;

use App\Magnet\MagnetGenerator;
use PHPUnit\Framework\Attributes\DataProvider;
use PHPUnit\Framework\TestCase;

class MagnetGeneratorTest extends TestCase
{
    private const TRACKERS = [
        'udp://tracker.example1.tld:1337/announce',
        'udp://tracker.example2.tld:6969/announce',
    ];

    public function testGeneratesBareMagnetLinkWithoutNameAndTrackers(): void
    {
        $generator = new MagnetGenerator(self::TRACKERS);

        self::assertSame(
            'magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567',
            $generator->generate('0123456789abcdef0123456789abcdef01234567', null, false),
        );
    }

    public function testAddsNameWhenProvided(): void
    {
        $generator = new MagnetGenerator();

        $link = $generator->generate('abc', 'Some torrent name', false);

        self::assertSame('magnet:?xt=urn:btih:abc&dn=Some+torrent+name', $link);
    }

    public function testAddsTrackersWhenEnabled(): void
    {
        $generator = new MagnetGenerator(self::TRACKERS);

        $link = $generator->generate('abc', null, true);

        self::assertSame(
            'magnet:?xt=urn:btih:abc'
                .'&tr=udp%3A%2F%2Ftracker.example1.tld%3A1337%2Fannounce'
                .'&tr=udp%3A%2F%2Ftracker.example2.tld%3A6969%2Fannounce',
            $link,
        );
    }

    public function testUrlencodesSpecialCharactersInHashAndName(): void
    {
        $generator = new MagnetGenerator();

        $link = $generator->generate('hash with spaces & symbols', 'name/with&specials', false);

        self::assertSame(
            'magnet:?xt=urn:btih:hash+with+spaces+%26+symbols&dn=name%2Fwith%26specials',
            $link,
        );
    }
}
