<?php
declare(strict_types=1);

namespace App\Twig;

use App\Helper\BsTreeviewFileTreeBuilder;
use Twig\Extension\AbstractExtension;
use Twig\{TwigFilter};

class FileTreeExtension extends AbstractExtension
{
    private BsTreeviewFileTreeBuilder $builder;

    public function __construct(BsTreeviewFileTreeBuilder $builder)
    {
        $this->builder = $builder;
    }

    public function getFilters(): array
    {
        return [
            new TwigFilter('file_tree', [$this->builder, 'buildFileTreeDataArray']),
            new TwigFilter('torrent_file_tree', [$this->builder, 'buildFileTreeDataArrayFromTorrent']),
        ];
    }
}
