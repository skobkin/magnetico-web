<?php
declare(strict_types=1);

namespace App\Controller;

use App\Magnet\MagnetGenerator;
use Symfony\Component\HttpFoundation\RedirectResponse;

class MagnetRedirectController
{
    public function redirect(string $infoHash, MagnetGenerator $magnetGenerator): RedirectResponse
    {
        return new RedirectResponse($magnetGenerator->generate($infoHash), RedirectResponse::HTTP_TEMPORARY_REDIRECT);
    }
}
