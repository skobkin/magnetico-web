<?php
declare(strict_types=1);

namespace App\Api\V1\Controller;

use App\Feed\RssGenerator;
use Symfony\Component\HttpFoundation\{Request, Response};
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Contracts\Cache\CacheInterface;
use Symfony\Contracts\Cache\ItemInterface;

class RssController extends AbstractController
{
    private const CONTENT_TYPE = 'application/rss+xml';
    private const CACHE_KEY = 'rss_last';
    private const CACHE_LIFETIME = 600;

    public function last(Request $request, RssGenerator $generator, CacheInterface $magneticodCache): Response
    {
        $page = (int) $request->query->get('page', '1');

        $xml = $magneticodCache->get(self::CACHE_KEY, function (ItemInterface $item) use ($generator, $page) {
            $item->expiresAfter(self::CACHE_LIFETIME);

            return $generator->generateLast($page);
        });

        return new Response($xml, 200, ['Content-Type' => self::CONTENT_TYPE]);
    }
}