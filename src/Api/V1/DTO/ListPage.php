<?php
declare(strict_types=1);

namespace App\Api\V1\DTO;

use Pagerfanta\Pagerfanta;
use Symfony\Component\Serializer\Annotation\Groups;

class ListPage
{
    #[Groups(['api'])]
    private int $numberOfPages;

    #[Groups(['api'])]
    private int $currentPage;

    #[Groups(['api'])]
    private int $numberOfResults;

    #[Groups(['api'])]
    private int $maxPerPage;

    #[Groups(['api'])]
    protected \Traversable $items;

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