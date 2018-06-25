<?php

namespace App\Api\V1\Controller;

use App\Api\V1\DTO\ListPage;
use App\Magnetico\Entity\Torrent;
use App\Magnetico\Repository\TorrentRepository;
use Pagerfanta\Adapter\DoctrineORMAdapter;
use Pagerfanta\Pagerfanta;
use Symfony\Component\HttpFoundation\{JsonResponse, Request};

class TorrentController extends AbstractApiController
{
    private const PER_PAGE = 20;

    public function search(Request $request, TorrentRepository $repo): JsonResponse
    {
        $query = $request->query->get('query', '');
        $page = (int) $request->query->get('page', '1');

        $pagerAdapter = new DoctrineORMAdapter($repo->createFindLikeQueryBuilder($query));
        $pager = new Pagerfanta($pagerAdapter);
        $pager
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