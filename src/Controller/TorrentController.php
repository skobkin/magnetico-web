<?php
declare(strict_types=1);

namespace App\Controller;

use App\Magnetico\Entity\Torrent;
use App\Search\TorrentSearcher;
use App\Pager\PagelessDoctrineORMAdapter;
use App\View\Torrent\FileTreeNode;
use Pagerfanta\Pagerfanta;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\HttpFoundation\{Request, Response};

class TorrentController extends AbstractController
{
    private const PER_PAGE = 20;

    public function searchTorrent(Request $request, TorrentSearcher $searcher): Response
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

        return $this->render('search_results.html.twig', [
            'torrents' => $pager,
            'searchQuery' => $query,
        ]);
    }

    public function showTorrent(Torrent $torrent): Response
    {
        return $this->render('torrent_show.html.twig', [
            'torrent' => $torrent,
            'files' => FileTreeNode::createFromTorrent($torrent),
        ]);
    }
}