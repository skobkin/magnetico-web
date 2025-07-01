<?php
declare(strict_types=1);

namespace App\Twig;

use App\Magnetico\Entity\Torrent;
use App\View\Torrent\FileTreeNode;
use Twig\Extension\AbstractExtension;
use Twig\TwigFilter;

class FileTreeExtension extends AbstractExtension
{
    public function getFilters(): array
    {
        return [
            new TwigFilter('create_file_tree', [self::class, 'createFileTree']),
        ];
    }

    /**
     * Converts a Torrent to a FileTreeNode for use in the file tree macro.
     */
    public static function createFileTree(Torrent $torrent): FileTreeNode
    {
        return FileTreeNode::createFromTorrent($torrent);
    }
}
