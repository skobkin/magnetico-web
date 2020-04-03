<?php

namespace App\Api\V1\Controller;

use App\Feed\RssGenerator;
use Symfony\Component\HttpFoundation\{Request, Response};
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;

class RssController extends AbstractController
{
    private const CONTENT_TYPE = 'application/rss+xml';

    public function last(Request $request, RssGenerator $generator): Response
    {
        $page = (int) $request->query->get('page', '1');

        $xml = $generator->generateLast($page);

        return new Response($xml, 200, ['Content-Type' => self::CONTENT_TYPE]);
    }
}