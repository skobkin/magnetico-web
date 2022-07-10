<?php
declare(strict_types=1);

namespace App\View\Torrent;

use App\Magnetico\Entity\{File, Torrent};

class FileTreeNode
{
    private ?string $name;

    private bool $isDir = true;

    private ?int $size;

    private ?FileTreeNode $parent;

    /** @var FileTreeNode[]|File[] */
    private array $children = [];

    private function __construct(string $name)
    {
        $this->name = $name;
    }

    public static function createFromTorrent(Torrent $torrent): FileTreeNode
    {
        $node = new static($torrent->getName());

        foreach ($torrent->getFiles() as $file) {
            $node->addFileToPath($file->getPath(), $file);
        }

        return $node;
    }

    public static function createFromFile(string $name, File $file, ?FileTreeNode $parent): FileTreeNode
    {
        $node = new static($name);
        $node->isDir = false;
        $node->size = $file->getSize();
        $node->parent = $parent;

        return $node;
    }

    public function addFileToPath(string $path, File $file): void
    {
        $path = ltrim($path, '/');

        $pathParts = explode('/', $path);

        // If we have file only file and not a tree.
        if (1 === count($pathParts)) {
            $this->addChild($path, self::createFromFile($path, $file, $this));

            return;
        }

        // If we have a file in a tree.
        $childNodeName = array_shift($pathParts);
        $childNodeChildPath = implode('/', $pathParts);

        if (!$this->hasChild($childNodeName)) {
            $childNode = new static($childNodeName);
            $childNode->parent = $this;
            $this->addChild($childNodeName, $childNode);
        } else {
            $childNode = $this->getChild($childNodeName);
        }

        $childNode->addFileToPath($childNodeChildPath, $file);
    }

    public function addChild(string $name, FileTreeNode $element, bool $overwriteDuplicates = false): void
    {
        if (!$overwriteDuplicates && array_key_exists($name, $this->children)) {
            throw new \RuntimeException(sprintf(
                'Child \'%s\' already exist.',
                $name
            ));
        }

        $this->children[$name] = $element;
    }

    public function hasChild(string $name): bool
    {
        return array_key_exists($name, $this->children);
    }

    public function getChild(string $name): File|FileTreeNode
    {
        if (!array_key_exists($name, $this->children)) {
            throw new \InvalidArgumentException(sprintf(
                'Node has no \'%s\' child.',
                $name
            ));
        }

        return $this->children[$name];
    }

    /**
     * @return File[]|FileTreeNode[]
     */
    public function getChildren(bool $dirsFirst = true): array
    {
        if (!$dirsFirst) {
            return $this->children;
        }

        $dirs = [];
        $files = [];

        foreach ($this->children as $name => $child) {
            if ($child instanceof self) {
                $dirs[$name] = $child;
            } elseif ($child instanceof File) {
                $files[] = $child;
            }
        }

        return array_merge($dirs, $files);
    }

    public function isDirectory(): bool
    {
        return $this->isDir;
    }

    public function countChildren(): int
    {
        return count($this->children);
    }

    public function getSize(): ?int
    {
        return $this->size;
    }
}
