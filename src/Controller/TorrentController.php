<?php

namespace App\Controller;

use App\Entity\Torrent;
use App\Repository\TorrentRepository;
use Pagerfanta\Adapter\DoctrineORMAdapter;
use Pagerfanta\Pagerfanta;
use Symfony\Bundle\FrameworkBundle\Controller\Controller;
use Symfony\Component\HttpFoundation\Request;
use Symfony\Component\HttpFoundation\Response;

class TorrentController extends Controller
{
    private const PER_PAGE = 20;

    public function searchTorrent(Request $request, TorrentRepository $repo): Response
    {
        $query = $request->query->get('query', '');
        $page = (int) $request->query->get('page', '1');

        $pagerAdapter = new DoctrineORMAdapter($repo->createFindLikeQueryBuilder($query));
        $pager = new Pagerfanta($pagerAdapter);
        $pager
            ->setCurrentPage($page)
            ->setMaxPerPage(self::PER_PAGE)
        ;

        return $this->render('search_results.html.twig', [
            'torrents' => $pager,
            'searchQuery' => $query,
        ]);
    }

    public function showTorrent(Torrent $torrent): Response
    {
        return $this->render('torrent_show.html.twig', [
            'torrent' => $torrent,
        ]);
    }
}