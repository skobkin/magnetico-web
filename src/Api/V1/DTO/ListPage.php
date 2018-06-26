<?php

namespace App\Api\V1\DTO;

use Pagerfanta\Pagerfanta;
use Symfony\Component\Serializer\Annotation as Serializer;

class ListPage
{
    /**
     * @var int
     *
     * @Serializer\Groups({"api"})
     */
    private $numberOfPages;

    /**
     * @var int
     *
     * @Serializer\Groups({"api"})
     */
    private $currentPage;

    /**
     * @var int
     *
     * @Serializer\Groups({"api"})
     */
    private $numberOfResults;

    /**
     * @var int
     *
     * @Serializer\Groups({"api"})
     */
    private $maxPerPage;

    /**
     * @var \Traversable
     *
     * @Serializer\Groups({"api"})
     */
    protected $items;

    public function __construct(\Traversable $items, int $numberOfResults, int $numberOfPages, int $currentPage, int $maxPerPage)
    {
        $this->items = $items;
        $this->numberOfResults = $numberOfResults;
        $this->numberOfPages = $numberOfPages;
        $this->currentPage = $currentPage;
        $this->maxPerPage = $maxPerPage;
    }

    public static function createFromPager(Pagerfanta $pager): self
    {
        return new static(
            $pager->getCurrentPageResults(),
            $pager->getNbResults(),
            $pager->getNbPages(),
            $pager->getCurrentPage(),
            $pager->getMaxPerPage()
        );
    }

    public function getNumberOfPages(): int
    {
        return $this->numberOfPages;
    }

    public function getCurrentPage(): int
    {
        return $this->currentPage;
    }

    public function getNumberOfResults(): int
    {
        return $this->numberOfResults;
    }

    public function getMaxPerPage(): int
    {
        return $this->maxPerPage;
    }

    public function getItems(): \Traversable
    {
        return $this->items;
    }
}