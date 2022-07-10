<?php
declare(strict_types=1);

namespace App\Magnet;

class MagnetGenerator
{
    private const MAGNET_TEMPLATE = 'magnet:?xt=urn:btih:%s';

    /** @var string[] Array of public trackers which will be added to the magnet link */
    private $publicTrackers = [];

    /**
     * @param string[] $publicTrackers
     */
    public function __construct(array $publicTrackers = [])
    {
        $this->publicTrackers = $publicTrackers;
    }

    public function generate(string $infoHash, ?string $name = null, bool $withTrackers = true): string
    {
        $url = sprintf(self::MAGNET_TEMPLATE, urlencode($infoHash));

        if (null !== $name) {
            $url .= '&dn='.urlencode($name);
        }

        if ($withTrackers) {
            foreach ($this->publicTrackers as $tracker) {
                $url .= '&tr='.urlencode($tracker);
            }
        }

        return $url;
    }
}
