<?php

namespace App\Controller;

use App\Repository\TorrentRepository;
use Symfony\Bundle\FrameworkBundle\Controller\Controller;
use Symfony\Component\HttpFoundation\Response;

class MainController extends Controller
{
    public function index(TorrentRepository $repo): Response
    {
        return $this->render('index.html.twig', [
            'torrentsCount' => $repo->getTorrentsTotalCount(),
        ]);
    }
}