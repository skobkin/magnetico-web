<?php
declare(strict_types=1);

namespace App\Twig;

use App\Magnet\MagnetGenerator;
use Twig\Extension\AbstractExtension;
use Twig\{TwigFilter, TwigFunction};

class MagnetExtension extends AbstractExtension
{
    public function __construct(
        private readonly MagnetGenerator $magnetGenerator
    ) {

    }

    public function getFunctions(): array
    {
        return [
            new TwigFunction('magnet', [$this->magnetGenerator, 'generate']),
        ];
    }

    public function getFilters(): array
    {
        return [
            new TwigFilter('magnet', [$this->magnetGenerator, 'generate']),
        ];
    }
}
