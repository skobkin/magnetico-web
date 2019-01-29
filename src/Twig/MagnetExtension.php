<?php

namespace App\Twig;

use App\Magnet\MagnetGenerator;
use Twig\Extension\AbstractExtension;
use Twig\{TwigFilter, TwigFunction};

class MagnetExtension extends AbstractExtension
{
    /** @var MagnetGenerator */
    private $magnetGenerator;

    public function __construct(MagnetGenerator $magnetGenerator)
    {
        $this->magnetGenerator = $magnetGenerator;
    }

    public function getFunctions(): array
    {
        return [
            new TwigFunction('magnet', [$this->magnetGenerator, 'generate']),
        ];
    }

    public function getFilters()
    {
        return [
            new TwigFilter('magnet', [$this->magnetGenerator, 'generate']),
        ];
    }
}