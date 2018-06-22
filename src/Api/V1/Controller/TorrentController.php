<?php

namespace App\Api\V1\Controller;

use App\Api\V1\DTO\ApiResponse;
use App\Entity\Torrent;
use App\Repository\TorrentRepository;
use Pagerfanta\Adapter\DoctrineORMAdapter;
use Pagerfanta\Pagerfanta;
use Symfony\Bundle\FrameworkBundle\Controller\Controller;
use Symfony\Component\HttpFoundation\{Request, Response};

class TorrentController extends Controller
{
    private const DEFAULT_SERIALIZER_GROUPS = ['api_v1'];

    private const PER_PAGE = 20;

    public function search(Request $request, TorrentRepository $repo): Response
    {
        $query = $request->query->get('query', '');
        $page = (int) $request->query->get('page', '1');

        $pagerAdapter = new DoctrineORMAdapter($repo->createFindLikeQueryBuilder($query));
        $pager = new Pagerfanta($pagerAdapter);
        $pager
            ->setCurrentPage($page)
            ->setMaxPerPage(self::PER_PAGE)
        ;

        return $this->json(new ApiResponse($pager->getCurrentPageResults()),Response::HTTP_OK, [], [
            'groups' => array_merge(self::DEFAULT_SERIALIZER_GROUPS,['api_v1_search']),
        ]);
    }

    public function show(Torrent $torrent): Response
    {
        return $this->json(new ApiResponse($torrent), Response::HTTP_OK, [], [
            'groups' => array_merge(self::DEFAULT_SERIALIZER_GROUPS,['api_v1_show']),
        ]);
    }




}