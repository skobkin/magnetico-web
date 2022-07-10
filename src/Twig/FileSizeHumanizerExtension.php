<?php
declare(strict_types=1);

namespace App\Twig;

use App\Helper\FileSizeHumanizer;
use Twig\Extension\AbstractExtension;
use Twig\TwigFilter;

class FileSizeHumanizerExtension extends AbstractExtension
{
    private FileSizeHumanizer $humanizer;

    public function __construct(FileSizeHumanizer $humanizer)
    {
        $this->humanizer = $humanizer;
    }

    public function getFilters(): array
    {
        return [
            new TwigFilter('humanize_size', [$this->humanizer, 'humanize']),
        ];
    }
}
