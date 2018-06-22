<?php

namespace App\Twig;

use Twig\Extension\AbstractExtension;
use Twig\TwigFunction;

class MagnetExtension extends AbstractExtension
{
    private const MAGNET_TEMPLATE = 'magnet:?xt=urn:btih:%s&dn=%s';

    /** @var string[] Array of public trackers which will be added to the magnet link */
    private $publicTrackers = [];

    /**
     * @param string[] $publicTrackers
     */
    public function __construct(array $publicTrackers = [])
    {
        $this->publicTrackers = $publicTrackers;
    }


    public function getFunctions(): array
    {
        return [
            new TwigFunction('magnet', [$this, 'createMagnet']),
        ];
    }

    public function createMagnet(string $name, string $infoHash, bool $addPublicTrackers = true): string
    {
        $magnetUrl = sprintf(self::MAGNET_TEMPLATE, urlencode($infoHash), urlencode($name));

        if ($addPublicTrackers) {
            foreach ($this->publicTrackers as $tracker) {
                $magnetUrl .= '&tr='.urlencode($tracker);
            }
        }

        return $magnetUrl;
    }
}