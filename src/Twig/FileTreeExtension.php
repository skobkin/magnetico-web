<?php

namespace App\Twig;

use App\Helper\BstreeviewFileTreeBuilder;
use Twig\Extension\AbstractExtension;
use Twig\{TwigFilter};

class FileTreeExtension extends AbstractExtension
{
    private BstreeviewFileTreeBuilder $builder;

    public function __construct(BstreeviewFileTreeBuilder $builder)
    {
        $this->builder = $builder;
    }

    public function getFilters()
    {
        return [
            new TwigFilter('file_tree', [$this->builder, 'buildFileTreeDataArray']),
            new TwigFilter('torrent_file_tree', [$this->builder, 'buildFileTreeDataArrayFromTorrent']),
        ];
    }
}
