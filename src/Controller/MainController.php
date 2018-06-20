<?php

namespace App\Controller;

use App\Repository\TorrentRepository;
use Symfony\Bundle\FrameworkBundle\Controller\Controller;

class MainController extends Controller
{
    public function index(TorrentRepository $repo)
    {
        return $this->render('index.html.twig', [
            'torrentsCount' => $repo->getTorrentsTotalCount(),
        ]);
    }
}