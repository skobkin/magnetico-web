<?php

namespace App\Controller;

use Symfony\Bundle\FrameworkBundle\Controller\Controller;
use Symfony\Component\HttpFoundation\Response;

class TestController extends Controller
{
    public function test(\App\Repository\Torrent $torrentRepo)
    {
        $torrents = $torrentRepo->getLastTorrents(10);

        return $this->render('torrent_list.html.twig', [
            'torrents' => $torrents,
        ]);
    }
}