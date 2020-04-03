<?php

namespace App\Api\V1\Controller;

use App\Api\V1\DTO\ListPage;
use App\Magnetico\Entity\Torrent;
use App\Pager\PagelessDoctrineORMAdapter;
use App\Search\TorrentSearcher;
use Pagerfanta\Pagerfanta;
use Symfony\Component\HttpFoundation\{JsonResponse, Request};

class TorrentController extends AbstractApiController
{
    private const PER_PAGE = 20;

    public function search(Request $request, TorrentSearcher $searcher): JsonResponse
    {
        $query = $request->query->get('query', '');
        $page = (int) $request->query->get('page', '1');
        $orderBy = $request->query->get('order-by');
        $order = $request->query->get('order', 'asc');

        $pagerAdapter = new PagelessDoctrineORMAdapter($searcher->createSearchQueryBuilder($query, $orderBy, $order));
        $pager = new Pagerfanta($pagerAdapter);
        $pager
            ->setAllowOutOfRangePages(true)
            ->setCurrentPage($page)
            ->setMaxPerPage(self::PER_PAGE)
        ;

        return $this->createJsonResponse(ListPage::createFromPager($pager), ['api_v1_search']);
    }

    public function show(Torrent $torrent): JsonResponse
    {
        return $this->createJsonResponse($torrent, ['api_v1_show'], JsonResponse::HTTP_OK,null, '');
    }




}